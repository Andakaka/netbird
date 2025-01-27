//go:build !android

package iptables

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/hashicorp/go-multierror"
	"github.com/nadoo/ipset"
	log "github.com/sirupsen/logrus"

	nberrors "github.com/netbirdio/netbird/client/errors"
	firewall "github.com/netbirdio/netbird/client/firewall/manager"
	"github.com/netbirdio/netbird/client/internal/acl/id"
	"github.com/netbirdio/netbird/client/internal/routemanager/refcounter"
	"github.com/netbirdio/netbird/client/internal/statemanager"
)

const (
	ipv4Nat = "netbird-rt-nat"
)

// constants needed to manage and create iptable rules
const (
	tableFilter             = "filter"
	tableNat                = "nat"
	chainPOSTROUTING        = "POSTROUTING"
	chainRTNAT              = "NETBIRD-RT-NAT"
	chainRTFWD              = "NETBIRD-RT-FWD"
	routingFinalForwardJump = "ACCEPT"
	routingFinalNatJump     = "MASQUERADE"

	matchSet = "--match-set"
)

type routeFilteringRuleParams struct {
	Sources     []netip.Prefix
	Destination netip.Prefix
	Proto       firewall.Protocol
	SPort       *firewall.Port
	DPort       *firewall.Port
	Direction   firewall.RuleDirection
	Action      firewall.Action
	SetName     string
}

type routeRules map[string][]string

type ipsetCounter = refcounter.Counter[string, []netip.Prefix, struct{}]

type router struct {
	iptablesClient   *iptables.IPTables
	rules            routeRules
	ipsetCounter     *ipsetCounter
	wgIface          iFaceMapper
	legacyManagement bool

	stateManager *statemanager.Manager
}

func newRouter(iptablesClient *iptables.IPTables, wgIface iFaceMapper) (*router, error) {
	r := &router{
		iptablesClient: iptablesClient,
		rules:          make(map[string][]string),
		wgIface:        wgIface,
	}

	r.ipsetCounter = refcounter.New(
		func(name string, sources []netip.Prefix) (struct{}, error) {
			return struct{}{}, r.createIpSet(name, sources)
		},
		func(name string, _ struct{}) error {
			return r.deleteIpSet(name)
		},
	)

	if err := ipset.Init(); err != nil {
		return nil, fmt.Errorf("init ipset: %w", err)
	}

	return r, nil
}

func (r *router) init(stateManager *statemanager.Manager) error {
	r.stateManager = stateManager

	if err := r.cleanUpDefaultForwardRules(); err != nil {
		log.Errorf("failed to clean up rules from FORWARD chain: %s", err)
	}

	if err := r.createContainers(); err != nil {
		return fmt.Errorf("create containers: %w", err)
	}

	r.updateState()

	return nil
}

func (r *router) AddRouteFiltering(
	sources []netip.Prefix,
	destination netip.Prefix,
	proto firewall.Protocol,
	sPort *firewall.Port,
	dPort *firewall.Port,
	action firewall.Action,
) (firewall.Rule, error) {
	ruleKey := id.GenerateRouteRuleKey(sources, destination, proto, sPort, dPort, action)
	if _, ok := r.rules[string(ruleKey)]; ok {
		return ruleKey, nil
	}

	var setName string
	if len(sources) > 1 {
		setName = firewall.GenerateSetName(sources)
		if _, err := r.ipsetCounter.Increment(setName, sources); err != nil {
			return nil, fmt.Errorf("create or get ipset: %w", err)
		}
	}

	params := routeFilteringRuleParams{
		Sources:     sources,
		Destination: destination,
		Proto:       proto,
		SPort:       sPort,
		DPort:       dPort,
		Action:      action,
		SetName:     setName,
	}

	rule := genRouteFilteringRuleSpec(params)
	if err := r.iptablesClient.Append(tableFilter, chainRTFWD, rule...); err != nil {
		return nil, fmt.Errorf("add route rule: %v", err)
	}

	r.rules[string(ruleKey)] = rule

	r.updateState()

	return ruleKey, nil
}

func (r *router) DeleteRouteRule(rule firewall.Rule) error {
	ruleKey := rule.GetRuleID()

	if rule, exists := r.rules[ruleKey]; exists {
		setName := r.findSetNameInRule(rule)

		if err := r.iptablesClient.Delete(tableFilter, chainRTFWD, rule...); err != nil {
			return fmt.Errorf("delete route rule: %v", err)
		}
		delete(r.rules, ruleKey)

		if setName != "" {
			if _, err := r.ipsetCounter.Decrement(setName); err != nil {
				return fmt.Errorf("failed to remove ipset: %w", err)
			}
		}
	} else {
		log.Debugf("route rule %s not found", ruleKey)
	}

	r.updateState()

	return nil
}

func (r *router) findSetNameInRule(rule []string) string {
	for i, arg := range rule {
		if arg == "-m" && i+3 < len(rule) && rule[i+1] == "set" && rule[i+2] == matchSet {
			return rule[i+3]
		}
	}
	return ""
}

func (r *router) createIpSet(setName string, sources []netip.Prefix) error {
	if err := ipset.Create(setName, ipset.OptTimeout(0)); err != nil {
		return fmt.Errorf("create set %s: %w", setName, err)
	}

	for _, prefix := range sources {
		if err := ipset.AddPrefix(setName, prefix); err != nil {
			return fmt.Errorf("add element to set %s: %w", setName, err)
		}
	}

	return nil
}

func (r *router) deleteIpSet(setName string) error {
	if err := ipset.Destroy(setName); err != nil {
		return fmt.Errorf("destroy set %s: %w", setName, err)
	}
	return nil
}

// AddNatRule inserts an iptables rule pair into the nat chain
func (r *router) AddNatRule(pair firewall.RouterPair) error {
	if r.legacyManagement {
		log.Warnf("This peer is connected to a NetBird Management service with an older version. Allowing all traffic for %s", pair.Destination)
		if err := r.addLegacyRouteRule(pair); err != nil {
			return fmt.Errorf("add legacy routing rule: %w", err)
		}
	}

	if !pair.Masquerade {
		return nil
	}

	if err := r.addNatRule(pair); err != nil {
		return fmt.Errorf("add nat rule: %w", err)
	}

	if err := r.addNatRule(firewall.GetInversePair(pair)); err != nil {
		return fmt.Errorf("add inverse nat rule: %w", err)
	}

	r.updateState()

	return nil
}

// RemoveNatRule removes an iptables rule pair from forwarding and nat chains
func (r *router) RemoveNatRule(pair firewall.RouterPair) error {
	if err := r.removeNatRule(pair); err != nil {
		return fmt.Errorf("remove nat rule: %w", err)
	}

	if err := r.removeNatRule(firewall.GetInversePair(pair)); err != nil {
		return fmt.Errorf("remove inverse nat rule: %w", err)
	}

	if err := r.removeLegacyRouteRule(pair); err != nil {
		return fmt.Errorf("remove legacy routing rule: %w", err)
	}

	r.updateState()

	return nil
}

// addLegacyRouteRule adds a legacy routing rule for mgmt servers pre route acls
func (r *router) addLegacyRouteRule(pair firewall.RouterPair) error {
	ruleKey := firewall.GenKey(firewall.ForwardingFormat, pair)

	if err := r.removeLegacyRouteRule(pair); err != nil {
		return err
	}

	rule := []string{"-s", pair.Source.String(), "-d", pair.Destination.String(), "-j", routingFinalForwardJump}
	if err := r.iptablesClient.Append(tableFilter, chainRTFWD, rule...); err != nil {
		return fmt.Errorf("add legacy forwarding rule %s -> %s: %v", pair.Source, pair.Destination, err)
	}

	r.rules[ruleKey] = rule

	return nil
}

func (r *router) removeLegacyRouteRule(pair firewall.RouterPair) error {
	ruleKey := firewall.GenKey(firewall.ForwardingFormat, pair)

	if rule, exists := r.rules[ruleKey]; exists {
		if err := r.iptablesClient.DeleteIfExists(tableFilter, chainRTFWD, rule...); err != nil {
			return fmt.Errorf("remove legacy forwarding rule %s -> %s: %v", pair.Source, pair.Destination, err)
		}
		delete(r.rules, ruleKey)
	} else {
		log.Debugf("legacy forwarding rule %s not found", ruleKey)
	}

	return nil
}

// GetLegacyManagement returns the current legacy management mode
func (r *router) GetLegacyManagement() bool {
	return r.legacyManagement
}

// SetLegacyManagement sets the route manager to use legacy management mode
func (r *router) SetLegacyManagement(isLegacy bool) {
	r.legacyManagement = isLegacy
}

// RemoveAllLegacyRouteRules removes all legacy routing rules for mgmt servers pre route acls
func (r *router) RemoveAllLegacyRouteRules() error {
	var merr *multierror.Error
	for k, rule := range r.rules {
		if !strings.HasPrefix(k, firewall.ForwardingFormatPrefix) {
			continue
		}
		if err := r.iptablesClient.DeleteIfExists(tableFilter, chainRTFWD, rule...); err != nil {
			merr = multierror.Append(merr, fmt.Errorf("remove legacy forwarding rule: %v", err))
		} else {
			delete(r.rules, k)
		}
	}

	r.updateState()

	return nberrors.FormatErrorOrNil(merr)
}

func (r *router) Reset() error {
	var merr *multierror.Error
	if err := r.cleanUpDefaultForwardRules(); err != nil {
		merr = multierror.Append(merr, err)
	}
	r.rules = make(map[string][]string)

	if err := r.ipsetCounter.Flush(); err != nil {
		merr = multierror.Append(merr, err)
	}

	r.updateState()

	return nberrors.FormatErrorOrNil(merr)
}

func (r *router) cleanUpDefaultForwardRules() error {
	err := r.cleanJumpRules()
	if err != nil {
		return err
	}

	log.Debug("flushing routing related tables")
	for _, chain := range []string{chainRTFWD, chainRTNAT} {
		table := r.getTableForChain(chain)

		ok, err := r.iptablesClient.ChainExists(table, chain)
		if err != nil {
			log.Errorf("failed check chain %s, error: %v", chain, err)
			return err
		} else if ok {
			err = r.iptablesClient.ClearAndDeleteChain(table, chain)
			if err != nil {
				log.Errorf("failed cleaning chain %s, error: %v", chain, err)
				return err
			}
		}
	}

	return nil
}

func (r *router) createContainers() error {
	for _, chain := range []string{chainRTFWD, chainRTNAT} {
		if err := r.createAndSetupChain(chain); err != nil {
			return fmt.Errorf("create chain %s: %w", chain, err)
		}
	}

	if err := r.insertEstablishedRule(chainRTFWD); err != nil {
		return fmt.Errorf("insert established rule: %w", err)
	}

	if err := r.addJumpRules(); err != nil {
		return fmt.Errorf("add jump rules: %w", err)
	}

	return nil
}

func (r *router) createAndSetupChain(chain string) error {
	table := r.getTableForChain(chain)

	if err := r.iptablesClient.NewChain(table, chain); err != nil {
		return fmt.Errorf("failed creating chain %s, error: %v", chain, err)
	}

	return nil
}

func (r *router) getTableForChain(chain string) string {
	if chain == chainRTNAT {
		return tableNat
	}
	return tableFilter
}

func (r *router) insertEstablishedRule(chain string) error {
	establishedRule := getConntrackEstablished()

	err := r.iptablesClient.Insert(tableFilter, chain, 1, establishedRule...)
	if err != nil {
		return fmt.Errorf("failed to insert established rule: %v", err)
	}

	ruleKey := "established-" + chain
	r.rules[ruleKey] = establishedRule

	return nil
}

func (r *router) addJumpRules() error {
	rule := []string{"-j", chainRTNAT}
	err := r.iptablesClient.Insert(tableNat, chainPOSTROUTING, 1, rule...)
	if err != nil {
		return err
	}
	r.rules[ipv4Nat] = rule

	return nil
}

func (r *router) cleanJumpRules() error {
	rule, found := r.rules[ipv4Nat]
	if found {
		err := r.iptablesClient.DeleteIfExists(tableNat, chainPOSTROUTING, rule...)
		if err != nil {
			return fmt.Errorf("failed cleaning rule from chain %s, err: %v", chainPOSTROUTING, err)
		}
	}

	return nil
}

func (r *router) addNatRule(pair firewall.RouterPair) error {
	ruleKey := firewall.GenKey(firewall.NatFormat, pair)

	if rule, exists := r.rules[ruleKey]; exists {
		if err := r.iptablesClient.DeleteIfExists(tableNat, chainRTNAT, rule...); err != nil {
			return fmt.Errorf("error while removing existing NAT rule for %s: %v", pair.Destination, err)
		}
		delete(r.rules, ruleKey)
	}

	rule := genRuleSpec(routingFinalNatJump, pair.Source, pair.Destination, r.wgIface.Name(), pair.Inverse)
	if err := r.iptablesClient.Append(tableNat, chainRTNAT, rule...); err != nil {
		return fmt.Errorf("error while appending new NAT rule for %s: %v", pair.Destination, err)
	}

	r.rules[ruleKey] = rule

	return nil
}

func (r *router) removeNatRule(pair firewall.RouterPair) error {
	ruleKey := firewall.GenKey(firewall.NatFormat, pair)

	if rule, exists := r.rules[ruleKey]; exists {
		if err := r.iptablesClient.DeleteIfExists(tableNat, chainRTNAT, rule...); err != nil {
			return fmt.Errorf("error while removing existing nat rule for %s: %v", pair.Destination, err)
		}

		delete(r.rules, ruleKey)
	} else {
		log.Debugf("nat rule %s not found", ruleKey)
	}

	return nil
}

func (r *router) updateState() {
	if r.stateManager == nil {
		return
	}

	var currentState *ShutdownState
	if existing := r.stateManager.GetState(currentState); existing != nil {
		if existingState, ok := existing.(*ShutdownState); ok {
			currentState = existingState
		}
	}
	if currentState == nil {
		currentState = &ShutdownState{}
	}

	currentState.Lock()
	defer currentState.Unlock()

	currentState.RouteRules = r.rules
	currentState.RouteIPsetCounter = r.ipsetCounter

	if err := r.stateManager.UpdateState(currentState); err != nil {
		log.Errorf("failed to update state: %v", err)
	}
}

func genRuleSpec(jump string, source, destination netip.Prefix, intf string, inverse bool) []string {
	intdir := "-i"
	lointdir := "-o"
	if inverse {
		intdir = "-o"
		lointdir = "-i"
	}
	return []string{intdir, intf, "!", lointdir, "lo", "-s", source.String(), "-d", destination.String(), "-j", jump}
}

func genRouteFilteringRuleSpec(params routeFilteringRuleParams) []string {
	var rule []string

	if params.SetName != "" {
		rule = append(rule, "-m", "set", matchSet, params.SetName, "src")
	} else if len(params.Sources) > 0 {
		source := params.Sources[0]
		rule = append(rule, "-s", source.String())
	}

	rule = append(rule, "-d", params.Destination.String())

	if params.Proto != firewall.ProtocolALL {
		rule = append(rule, "-p", strings.ToLower(string(params.Proto)))
		rule = append(rule, applyPort("--sport", params.SPort)...)
		rule = append(rule, applyPort("--dport", params.DPort)...)
	}

	rule = append(rule, "-j", actionToStr(params.Action))

	return rule
}

func applyPort(flag string, port *firewall.Port) []string {
	if port == nil {
		return nil
	}

	if port.IsRange && len(port.Values) == 2 {
		return []string{flag, fmt.Sprintf("%d:%d", port.Values[0], port.Values[1])}
	}

	if len(port.Values) > 1 {
		portList := make([]string, len(port.Values))
		for i, p := range port.Values {
			portList[i] = strconv.Itoa(p)
		}
		return []string{"-m", "multiport", flag, strings.Join(portList, ",")}
	}

	return []string{flag, strconv.Itoa(port.Values[0])}
}
