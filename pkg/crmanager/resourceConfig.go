/*-
* Copyright (c) 2016-2019, F5 Networks, Inc.
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*
*    http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
 */

package crmanager

import (
	"bytes"
	"fmt"
	"net"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cisapiv1 "github.com/F5Networks/k8s-bigip-ctlr/config/apis/cis/v1"
	log "github.com/F5Networks/k8s-bigip-ctlr/pkg/vlogger"
)

// NewResources is Constructor for Resources
func NewResources() *Resources {
	var rs Resources
	rs.Init()
	return &rs
}

// Resources is Map of Resource configs
type Resources struct {
	sync.Mutex
	rm       resourceKeyMap
	rsMap    ResourceConfigMap
	objDeps  ObjectDependencyMap
	oldRsMap ResourceConfigMap
}

// Init is Receiver to initialize the object.
func (rs *Resources) Init() {
	rs.rm = make(resourceKeyMap)
	rs.rsMap = make(ResourceConfigMap)
	rs.objDeps = make(ObjectDependencyMap)
	rs.oldRsMap = make(ResourceConfigMap)
}

type mergedRuleEntry struct {
	RuleName       string
	OtherRuleNames []string
	MergedActions  map[string][]*action
	OriginalRule   *Rule
}

// Key is resource name, value is unused (since go doesn't have set objects).
type resourceList map[string]bool

// Key is namespace/servicename/serviceport, value is map of resources.
type resourceKeyMap map[serviceKey]resourceList

// ResourceConfigMap key is resource name, value is pointer to config. May be shared.
type ResourceConfigMap map[string]*ResourceConfig

// ObjectDependency TODO => dep can be replaced with  internal DS rqkey
// ObjectDependency identifies a K8s Object
type ObjectDependency struct {
	Kind      string
	Namespace string
	Name      string
	Service   string
}

// ObjectDependencyMap key is a VirtualServer and the value is a
// map of other objects it depends on - typically services.
type ObjectDependencyMap map[ObjectDependency]ObjectDependencies

// RuleDep defines the rule for choosing a service from multiple services in VirtualServer, mainly by path.
const RuleDep = "Rule"

const (
	DEFAULT_MODE       string = "tcp"
	DEFAULT_BALANCE    string = "round-robin"
	DEFAULT_HTTP_PORT  int32  = 80
	DEFAULT_HTTPS_PORT int32  = 443

	urlRewriteRulePrefix      = "url-rewrite-rule-"
	appRootForwardRulePrefix  = "app-root-forward-rule-"
	appRootRedirectRulePrefix = "app-root-redirect-rule-"

	// Indicator to use an F5 schema
	schemaIndicator string = "f5schemadb://"

	// Constants for CustomProfile.Type as defined in CCCL
	CustomProfileAll    string = "all"
	CustomProfileClient string = "clientside"
	CustomProfileServer string = "serverside"

	// Constants for CustomProfile.PeerCertMode
	PeerCertRequired = "require"
	PeerCertIgnored  = "ignore"
	PeerCertDefault  = PeerCertIgnored

	// Constants
	HttpRedirectIRuleName = "http_redirect_irule"
	// Internal data group for https redirect
	HttpsRedirectDgName = "https_redirect_dg"
)

// constants for TLS references
const (
	// reference for profiles stored in BIG-IP
	BIGIP = "bigip"
	// reference for profiles stores as secrets in k8s cluster
	Secret = "secret"
)

// ObjectDependencies contains each dependency and its use count (usually 1)
type ObjectDependencies map[ObjectDependency]int

// Store of CustomProfiles
type CustomProfileStore struct {
	sync.Mutex
	Profs map[SecretKey]CustomProfile
}

func NewCustomProfile(
	profile ProfileRef,
	cert,
	key,
	serverName string,
	sni bool,
	peerCertMode,
	caFile string,
) CustomProfile {
	cp := CustomProfile{
		Name:         profile.Name,
		Partition:    profile.Partition,
		Context:      profile.Context,
		Cert:         cert,
		Key:          key,
		ServerName:   serverName,
		SNIDefault:   sni,
		PeerCertMode: peerCertMode,
	}
	if peerCertMode == PeerCertRequired {
		cp.CAFile = caFile
	}
	return cp
}

// NewCustomProfiles is a Constructor for CustomProfiles
func NewCustomProfiles() *CustomProfileStore {
	var cps CustomProfileStore
	cps.Profs = make(map[SecretKey]CustomProfile)
	return &cps
}

func NewIRule(name, partition, code string) *IRule {
	return &IRule{
		Name:      name,
		Partition: partition,
		Code:      code,
	}
}

// Creates an IRule if it doesn't already exist
func (crMgr *CRManager) addIRule(name, partition, rule string) {
	crMgr.irulesMutex.Lock()
	defer crMgr.irulesMutex.Unlock()

	key := NameRef{
		Name:      name,
		Partition: partition,
	}
	if _, found := crMgr.irulesMap[key]; !found {
		crMgr.irulesMap[key] = NewIRule(name, partition, rule)
	}
}

// Creates an InternalDataGroup if it doesn't already exist
func (crMgr *CRManager) addInternalDataGroup(name, partition string) {
	crMgr.intDgMutex.Lock()
	defer crMgr.intDgMutex.Unlock()

	key := NameRef{
		Name:      name,
		Partition: partition,
	}
	if _, found := crMgr.intDgMap[key]; !found {
		crMgr.intDgMap[key] = make(DataGroupNamespaceMap)
	}
}

func JoinBigipPath(partition, objName string) string {
	if objName == "" {
		return ""
	}
	if partition == "" {
		return objName
	}
	return fmt.Sprintf("/%s/%s", partition, objName)
}

// Adds an IRule reference to a Virtual object
func (v *Virtual) AddIRule(ruleName string) bool {
	for _, irule := range v.IRules {
		if irule == ruleName {
			return false
		}
	}
	v.IRules = append(v.IRules, ruleName)
	return true
}

// NewObjectDependencies parses an object and returns a map of its dependencies
func NewObjectDependencies(
	obj interface{},
) (ObjectDependency, ObjectDependencies) {
	deps := make(ObjectDependencies)
	virtual := obj.(*cisapiv1.VirtualServer)
	// TODO => dep can be replaced with  internal DS rqkey
	key := ObjectDependency{
		Kind:      VirtualServer,
		Name:      virtual.ObjectMeta.Name,
		Namespace: virtual.ObjectMeta.Namespace,
	}

	deps[key] = 1
	for _, pool := range virtual.Spec.Pools {
		dep := ObjectDependency{
			Kind:      RuleDep,
			Namespace: virtual.ObjectMeta.Namespace,
			Name:      virtual.Spec.Host + pool.Path,
			Service:   pool.Service,
		}
		deps[dep]++
	}
	return key, deps
}

type portStruct struct {
	protocol string
	port     int32
}

func (slice ProfileRefs) Less(i, j int) bool {
	return ((slice[i].Partition < slice[j].Partition) ||
		(slice[i].Partition == slice[j].Partition &&
			slice[i].Name < slice[j].Name))
}

func (slice ProfileRefs) Len() int {
	return len(slice)
}

func (slice ProfileRefs) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

// Return the required ports for VS (depending on sslRedirect/allowHttp vals)
func (crMgr *CRManager) virtualPorts(vs *cisapiv1.VirtualServer) []portStruct {

	// TODO: Support Custom ports
	const DEFAULT_HTTP_PORT int32 = 80
	const DEFAULT_HTTPS_PORT int32 = 443
	var httpPort int32
	var httpsPort int32
	httpPort = DEFAULT_HTTP_PORT
	httpsPort = DEFAULT_HTTPS_PORT

	http := portStruct{
		protocol: "http",
		port:     httpPort,
	}

	https := portStruct{
		protocol: "https",
		port:     httpsPort,
	}
	var ports []portStruct

	if 0 != len(vs.Spec.TLSProfileName) {
		// 2 virtual servers needed, both HTTP and HTTPS
		ports = append(ports, http)
		ports = append(ports, https)
	} else {
		// HTTP only
		ports = append(ports, http)
	}

	return ports
}

// format the virtual server name for an VirtualServer
func formatVirtualServerName(ip string, port int32) string {
	// Strip any bracket characters; replace special characters ". : /"
	// with "-" and "%" with ".", for naming purposes
	ip = strings.Trim(ip, "[]")
	ip = AS3NameFormatter(ip)
	return fmt.Sprintf("f5_crd_virtualserver_%s_%d", ip, port)
}

// format the pool name for an VirtualServer
func formatVirtualServerPoolName(namespace, svc string, nodeMemberLabel string) string {
	poolName := fmt.Sprintf("%s_%s", namespace, svc)
	if nodeMemberLabel != "" {
		poolName = fmt.Sprintf("%s_%s", poolName, nodeMemberLabel)
	}
	return AS3NameFormatter(poolName)
}

// Creates resource config based on VirtualServer resource config
func (crMgr *CRManager) createRSConfigFromVirtualServer(
	vs *cisapiv1.VirtualServer,
	pStruct portStruct,
) *ResourceConfig {

	var cfg ResourceConfig
	var bindAddr string
	var pools Pools
	var rules *Rules
	var plcy *Policy

	cfg.Virtual.Partition = crMgr.Partition

	if vs.Spec.VirtualServerAddress == "" {
		// Virtual Server IP is not given, exit with error log.
		log.Error("VirtualServer IP Address is not provided.  " +
			"Create VirtualServer with 'virtual.spec.VirtualServerAddress'.")
	} else {
		bindAddr = vs.Spec.VirtualServerAddress
	}
	// Create VirtualServer in resource config.
	cfg.Virtual.Name = formatVirtualServerName(bindAddr, pStruct.port)

	for _, pl := range vs.Spec.Pools {
		pool := Pool{
			Name: formatVirtualServerPoolName(
				vs.ObjectMeta.Namespace,
				pl.Service,
				pl.NodeMemberLabel,
			),
			Partition:       cfg.Virtual.Partition,
			ServiceName:     pl.Service,
			ServicePort:     pl.ServicePort,
			NodeMemberLabel: pl.NodeMemberLabel,
		}
		pools = append(pools, pool)
	}

	rules = processVirtualServerRules(vs)

	policyName := cfg.Virtual.Name + "_policy"

	plcy = createPolicy(*rules, policyName, vs.ObjectMeta.Namespace)

	cfg.MetaData.rscName = vs.ObjectMeta.Name

	cfg.MetaData.ResourceType = VirtualServer
	cfg.Virtual.Enabled = true
	cfg.Virtual.SetVirtualAddress(bindAddr, pStruct.port)
	cfg.Pools = append(cfg.Pools, pools...)
	if plcy != nil {
		cfg.SetPolicy(*plcy)
	}

	// If virtual server already exists with same name, it gets overridden
	crMgr.resources.rsMap[cfg.Virtual.Name] = &cfg
	return &cfg
}

// handleVirtualServerTLS handles TLS configuration for the Virtual Server resource
// Return value is whether or not a custom profile was updated
func (crMgr *CRManager) handleVirtualServerTLS(
	rsCfg *ResourceConfig,
	vs *cisapiv1.VirtualServer,
	svcFwdRulesMap ServiceFwdRuleMap,
) bool {
	if 0 == len(vs.Spec.TLSProfileName) {
		// Probably this is a non-tls Virtual Server, nothing to do w.r.t TLS
		return false
	}

	var httpsPort int32
	httpsPort = DEFAULT_HTTPS_PORT

	// If we are processing the HTTPS server,
	// then we don't need a redirect policy, only profiles
	if rsCfg.Virtual.VirtualAddress.Port == httpsPort {
		// Virtual Server related properties
		// Virtual Server and TLSProfile are assumed to be in same namespace
		vsNamespace := vs.ObjectMeta.Namespace
		vsName := vs.ObjectMeta.Name

		// TLSProfile name
		tlsName := vs.Spec.TLSProfileName
		tlsKey := fmt.Sprintf("%s/%s", vsNamespace, tlsName)

		// Initialize CustomResource Informer for required namespace
		crInf, ok := crMgr.getNamespaceInformer(vsNamespace)
		if !ok {
			log.Errorf("Informer not found for namespace: %v", vsNamespace)
			return false
		}

		// TODO: Create Internal Structure to hold TLSProfiles. Make API call only for a new TLSProfile
		// Check if the TLSProfile exists and valid for us.
		tlsInterface, tlsFound, _ := crInf.tsInformer.GetIndexer().GetByKey(tlsKey)
		if !tlsFound {
			log.Infof("TLSProfile %s is invalid", tlsName)
			return false
		}

		// TLSProfile Object
		tls := tlsInterface.(*cisapiv1.TLSProfile)

		// Process Profile
		switch tls.Spec.TLS.Reference {
		case BIGIP:
			clientSSL := tls.Spec.TLS.ClientSSL
			serverSSL := tls.Spec.TLS.ServerSSL
			// Profile is a BIG-IP default
			log.Debugf("Processing BIGIP referenced profiles for Virtual '%s' using TLSProfile '%s'",
				vsName, tlsName)
			// Process referenced BIG-IP clientSSL
			if clientSSL != "" {
				clientProfRef := ConvertStringToProfileRef(
					clientSSL, CustomProfileClient, vsNamespace)
				rsCfg.Virtual.AddOrUpdateProfile(clientProfRef)
			}
			// Process referenced BIG-IP serverSSL
			if serverSSL != "" {
				serverProfRef := ConvertStringToProfileRef(
					serverSSL, CustomProfileServer, vsNamespace)
				rsCfg.Virtual.AddOrUpdateProfile(serverProfRef)
			}
			log.Debugf("Updated BIGIP referenced profiles for Virtual '%s' using TLSProfile '%s'",
				vsName, tlsName)
			return true
		case Secret:
			// Prepare SSL Transient Context
			// Check if TLS Secret already exists
			clientSSL := tls.Spec.TLS.ClientSSL
			if secret, ok := crMgr.SSLContext[clientSSL]; ok {
				log.Debugf("TLSProfile '%s' is already available with CIS in SSLContext",
					tlsName)
				err, _ := crMgr.createSecretSslProfile(rsCfg, secret)
				if err != nil {
					log.Debugf("error %v encountered for '%s' using TLSProfile '%s'",
						err, vsName, tlsName)
					return false
				}
			} else {
				// Check if profile is contained in a Secret
				// Update the SSL Context if secret found, This is used to avoid api calls
				log.Debugf("TLSProfile '%s' does not exist with CIS in SSLContext, Store for further use",
					tlsName)
				secret, err := crMgr.kubeClient.CoreV1().Secrets(vsNamespace).
					Get(clientSSL, metav1.GetOptions{})
				if err != nil {
					log.Debugf("secret %s not found for Virtual '%s' using TLSProfile '%s'",
						clientSSL, vsName, tlsName)
					return false
				}
				crMgr.SSLContext[clientSSL] = secret
				error, _ := crMgr.createSecretSslProfile(rsCfg, secret)
				if error != nil {
					log.Debugf("error %v encountered for '%s' using TLSProfile '%s'",
						error, vsName, tlsName)
					return false
				}
			}
			profRef := ProfileRef{
				Partition: rsCfg.Virtual.Partition,
				Name:      clientSSL,
				Context:   CustomProfileClient,
				Namespace: vsNamespace,
			}
			rsCfg.Virtual.AddOrUpdateProfile(profRef)
			return true
		default:
			log.Errorf("referenced profile does not exist for Virtual '%s' using TLSProfile '%s'",
				vsName, tlsName)
			return false
		}
	}

	// httpTraffic defines the behaviour of http Virtual Server on BIG-IP
	// Possible values are allow, none and redirect
	httpTraffic := vs.Spec.HTTPTraffic
	if httpTraffic != "" {
		// -----------------------------------------------------------------
		// httpTraffic = allow -> Allows HTTP
		// httpTraffic = none  -> Only HTTPS
		// httpTraffic = redirect -> redirects HTTP to HTTPS
		// -----------------------------------------------------------------
		if httpTraffic == "redirect" {
			// set HTTP redirect iRule
			log.Debugf("Applying HTTP redirect iRule.")
			ruleName := fmt.Sprintf("%s_%d", HttpRedirectIRuleName, httpsPort)
			crMgr.addIRule(ruleName, DEFAULT_PARTITION, httpRedirectIRule(httpsPort))
			crMgr.addInternalDataGroup(HttpsRedirectDgName, DEFAULT_PARTITION)
			ruleName = JoinBigipPath(DEFAULT_PARTITION, ruleName)
			rsCfg.Virtual.AddIRule(ruleName)
			host := vs.Spec.Host
			for _, pool := range vs.Spec.Pools {
				svcFwdRulesMap.AddEntry(vs.ObjectMeta.Namespace,
					pool.Service, host, pool.Path)
			}
		} else if httpTraffic == "allow" {
			// State 3, do not apply any policy
			log.Debugf("[CORE] TLS: Not applying any policies.")
		}
	}

	return false
}

// ConvertStringToProfileRef converts strings to profile references
func ConvertStringToProfileRef(profileName, context, ns string) ProfileRef {
	profName := strings.TrimSpace(strings.TrimPrefix(profileName, "/"))
	parts := strings.Split(profName, "/")
	profRef := ProfileRef{Context: context, Namespace: ns}
	switch len(parts) {
	case 2:
		profRef.Partition = parts[0]
		profRef.Name = parts[1]
	case 1:
		log.Debugf("[RESOURCE] Partition not provided in profile '%s', using default partition '%s'",
			profileName, DEFAULT_PARTITION)
		profRef.Partition = DEFAULT_PARTITION
		profRef.Name = profileName
	default:
		// This is almost certainly an error, but again issue a warning for
		// improved context here and pass it through to be handled elsewhere.
		log.Warningf("[RESOURCE] Profile name '%v' is formatted incorrectly.", profileName)
	}
	return profRef
}

// AddOrUpdateProfile updates profile to rsCfg
func (v *Virtual) AddOrUpdateProfile(prof ProfileRef) bool {
	// The profiles are maintained as a sorted array.
	// The profiles are maintained as a sorted array.
	keyFunc := func(i int) bool {
		return ((v.Profiles[i].Partition > prof.Partition) ||
			(v.Profiles[i].Partition == prof.Partition &&
				v.Profiles[i].Name >= prof.Name))
	}
	profCt := v.Profiles.Len()
	i := sort.Search(profCt, keyFunc)
	if i < profCt && v.Profiles[i].Partition == prof.Partition &&
		v.Profiles[i].Name == prof.Name {
		// found, look for data changed
		if v.Profiles[i].Context == prof.Context {
			// unchanged
			return false
		}
	} else {
		// Insert into the correct position.
		v.Profiles = append(v.Profiles, ProfileRef{})
		copy(v.Profiles[i+1:], v.Profiles[i:])
	}
	v.Profiles[i] = prof

	return true
}

// SetVirtualAddress sets a VirtualAddress
func (v *Virtual) SetVirtualAddress(bindAddr string, port int32) {
	v.Destination = ""
	if bindAddr == "" && port == 0 {
		v.VirtualAddress = nil
	} else {
		v.VirtualAddress = &virtualAddress{
			BindAddr: bindAddr,
			Port:     port,
		}
		// Validate the IP address, and create the destination
		ip, rd := split_ip_with_route_domain(bindAddr)
		if len(rd) > 0 {
			rd = "%" + rd
		}
		addr := net.ParseIP(ip)
		if nil != addr {
			var format string
			if nil != addr.To4() {
				format = "/%s/%s%s:%d"
			} else {
				format = "/%s/%s%s.%d"
			}
			v.Destination = fmt.Sprintf(format, v.Partition, ip, rd, port)
		}
	}
}

// AddRuleToPolicy adds a new rule to existing policy
func (rc *ResourceConfig) AddRuleToPolicy(
	policyName string,
	rule *Rule,
) {
	// We currently have at most 1 policy, 'forwarding'
	policy := rc.FindPolicy("forwarding")
	if nil != policy {
		foundMatch := false
		for i, r := range policy.Rules {
			if r.Name == rule.Name && r.FullURI == rule.FullURI {
				// Replace old rule with new rule, but make sure Ordinal is correct.
				foundMatch = true
				rule.Ordinal = r.Ordinal
				policy.Rules[i] = rule
				break
			}
		}
		if !foundMatch {
			// Rule not found, add.
			policy.Rules = append(policy.Rules, rule)
		}
	} else {
		policy = createPolicy(Rules{rule}, policyName, rc.Virtual.Partition)
	}
	rc.SetPolicy(*policy)
}

// SetPolicy sets a policy
func (rc *ResourceConfig) SetPolicy(policy Policy) {
	toFind := nameRef{
		Name:      policy.Name,
		Partition: policy.Partition,
	}
	found := false
	for _, polName := range rc.Virtual.Policies {
		if reflect.DeepEqual(toFind, polName) {
			found = true
			break
		}
	}
	if !found {
		rc.Virtual.Policies = append(rc.Virtual.Policies, toFind)
	}
	for i, pol := range rc.Policies {
		if pol.Name == policy.Name && pol.Partition == policy.Partition {
			rc.Policies[i] = policy
			return
		}
	}
	rc.Policies = append(rc.Policies, policy)
}

// FindPolicy gets the information of a policy
func (rc *ResourceConfig) FindPolicy(controlType string) *Policy {
	for _, pol := range rc.Policies {
		for _, cType := range pol.Controls {
			if cType == controlType {
				return &pol
			}
		}
	}
	return nil
}

// GetByName gets a specific Resource cfg
func (rs *Resources) GetByName(name string) (*ResourceConfig, bool) {
	resource, ok := rs.rsMap[name]
	return resource, ok
}

// GetAllResources is list of all resource configs
func (rs *Resources) GetAllResources() ResourceConfigs {
	var cfgs ResourceConfigs
	for _, cfg := range rs.rsMap {
		cfgs = append(cfgs, cfg)
	}
	return cfgs
}

// Copies from an existing config into our new config
func (rc *ResourceConfig) copyConfig(cfg *ResourceConfig) {
	// MetaData
	rc.MetaData = cfg.MetaData
	// Virtual
	rc.Virtual = cfg.Virtual
	// Policies ref
	rc.Virtual.Policies = make([]nameRef, len(cfg.Virtual.Policies))
	copy(rc.Virtual.Policies, cfg.Virtual.Policies)
	// Pools
	rc.Pools = make(Pools, len(cfg.Pools))
	copy(rc.Pools, cfg.Pools)
	// Pool Members and Monitor Names
	for i := range rc.Pools {
		rc.Pools[i].Members = make([]Member, len(cfg.Pools[i].Members))
		copy(rc.Pools[i].Members, cfg.Pools[i].Members)
	}
	// Policies
	rc.Policies = make([]Policy, len(cfg.Policies))
	copy(rc.Policies, cfg.Policies)

	for i := range rc.Policies {
		rc.Policies[i].Controls = make([]string, len(cfg.Policies[i].Controls))
		copy(rc.Policies[i].Controls, cfg.Policies[i].Controls)
		rc.Policies[i].Requires = make([]string, len(cfg.Policies[i].Requires))
		copy(rc.Policies[i].Requires, cfg.Policies[i].Requires)

		// Rules
		rc.Policies[i].Rules = make([]*Rule, len(cfg.Policies[i].Rules))
		// Actions and Conditions
		for j := range rc.Policies[i].Rules {
			rc.Policies[i].Rules[j] = &Rule{}
			rc.Policies[i].Rules[j].Actions = make([]*action, len(cfg.Policies[i].Rules[j].Actions))
			rc.Policies[i].Rules[j].Conditions = make([]*condition, len(cfg.Policies[i].Rules[j].Conditions))
			for k := range rc.Policies[i].Rules[j].Conditions {
				rc.Policies[i].Rules[j].Conditions[k] = &condition{}
				rc.Policies[i].Rules[j].Conditions[k].Values =
					make([]string, len(cfg.Policies[i].Rules[j].Conditions[k].Values))
			}
		}
		copy(rc.Policies[i].Rules, cfg.Policies[i].Rules)
		for j := range rc.Policies[i].Rules {
			copy(rc.Policies[i].Rules[j].Actions, cfg.Policies[i].Rules[j].Actions)
			copy(rc.Policies[i].Rules[j].Conditions, cfg.Policies[i].Rules[j].Conditions)
			for k := range rc.Policies[i].Rules[j].Conditions {
				copy(rc.Policies[i].Rules[j].Conditions[k].Values, cfg.Policies[i].Rules[j].Conditions[k].Values)
			}
		}
	}
}

// split_ip_with_route_domain splits ip into ip and route domain
func split_ip_with_route_domain(address string) (ip string, rd string) {
	// Split the address into the ip and routeDomain (optional) parts
	//     address is of the form: <ipv4_or_ipv6>[%<routeDomainID>]
	idRdRegex := regexp.MustCompile(`^([^%]*)%(\d+)$`)

	match := idRdRegex.FindStringSubmatch(address)
	if match != nil {
		ip = match[1]
		rd = match[2]
	} else {
		ip = address
		rd = ""
	}
	return
}

// UpdateDependencies will keep the rs.objDeps map updated, and return two
// arrays identifying what has changed - added for dependencies that were
// added, and removed for dependencies that were removed.
func (rs *Resources) UpdateDependencies(
	newKey ObjectDependency,
	newDeps ObjectDependencies,
	lookupFunc func(key ObjectDependency) bool,
) ([]ObjectDependency, []ObjectDependency) {

	// Update dependencies for newKey
	var added, removed []ObjectDependency
	oldDeps, found := rs.objDeps[newKey]
	if found {
		// build list of removed deps
		for oldDep := range oldDeps {
			if _, found = newDeps[oldDep]; !found {
				// If Rule, put at front of list to ensure we unmerge before trying
				// to process any removed annotation rules
				if oldDep.Kind == RuleDep {
					removed = append([]ObjectDependency{oldDep}, removed...)
				} else {
					removed = append(removed, oldDep)
				}
			}
		}
		// build list of added deps
		for newDep := range newDeps {
			if _, found = oldDeps[newDep]; !found {
				added = append(added, newDep)
			}
		}
	} else {
		// all newDeps are adds
		for newDep := range newDeps {
			added = append(added, newDep)
		}
	}
	rs.objDeps[newKey] = newDeps

	return added, removed
}

func (rc *ResourceConfig) DeleteRuleFromPolicy(
	policyName string,
	rule *Rule,
	mergedRulesMap map[string]map[string]mergedRuleEntry,
) {
	var policy *Policy
	for _, pol := range rc.Policies {
		if pol.Name == policyName {
			policy = &pol
			break
		}
	}
	if nil != policy {
		for i, r := range policy.Rules {
			if r.Name == rule.Name && r.FullURI == rule.FullURI {
				// Remove old rule
				unmerged := rc.UnmergeRule(rule.Name, mergedRulesMap)
				if len(policy.Rules) == 1 && !unmerged {
					rc.RemovePolicy(*policy)
				} else if !unmerged {
					ruleOffsets := []int{i}
					policy.RemoveRules(ruleOffsets)
					rc.SetPolicy(*policy)
				}
				break
			}
		}
	}
}

//TODO ==> To be implemented Post Alpha.
//Delete unused pool from resource config
// Updating or removing the service from virtual may required delete unused pool from rscfg.
// func (rc *ResourceConfig) DeleteUnusedPool(){
// }

func (rc *ResourceConfig) RemovePolicy(policy Policy) {
	toFind := nameRef{
		Name:      policy.Name,
		Partition: policy.Partition,
	}
	for i, polName := range rc.Virtual.Policies {
		if reflect.DeepEqual(toFind, polName) {
			// Remove from array
			copy(rc.Virtual.Policies[i:], rc.Virtual.Policies[i+1:])
			rc.Virtual.Policies[len(rc.Virtual.Policies)-1] = nameRef{}
			rc.Virtual.Policies = rc.Virtual.Policies[:len(rc.Virtual.Policies)-1]
			break
		}
	}
	for i, pol := range rc.Policies {
		if pol.Name == toFind.Name && pol.Partition == toFind.Partition {
			if len(rc.Policies) == 1 {
				// No policies left
				rc.Policies = nil
			} else {
				// Remove from array
				copy(rc.Policies[i:], rc.Policies[i+1:])
				rc.Policies[len(rc.Policies)-1] = Policy{}
				rc.Policies = rc.Policies[:len(rc.Policies)-1]
			}
			return
		}
	}
}

func (pol *Policy) RemoveRules(ruleOffsets []int) bool {
	polChanged := false
	if len(ruleOffsets) > 0 {
		polChanged = true
		for i := len(ruleOffsets) - 1; i >= 0; i-- {
			pol.RemoveRuleAt(ruleOffsets[i])
		}
		// Must fix the ordinals on the remaining rules
		for i, rule := range pol.Rules {
			rule.Ordinal = i
		}
	}
	return polChanged
}

func (pol *Policy) RemoveRuleAt(offset int) bool {
	if offset >= len(pol.Rules) {
		return false
	}
	copy(pol.Rules[offset:], pol.Rules[offset+1:])
	pol.Rules[len(pol.Rules)-1] = &Rule{}
	pol.Rules = pol.Rules[:len(pol.Rules)-1]
	return true
}

func (rc *ResourceConfig) UnmergeRule(ruleName string, mergedRulesMap map[string]map[string]mergedRuleEntry) bool {
	rsName := rc.GetName()
	if _, ok := mergedRulesMap[rsName]; !ok {
		return false
	}
	if _, ok := mergedRulesMap[rsName][ruleName]; !ok {
		return false
	}
	entry := mergedRulesMap[rsName][ruleName]

	// This rule had other rules merged into it
	if entry.MergedActions != nil {
		// Find the policy and rule for this entry and delete it
		policy := rc.FindPolicy("forwarding")
		for i := range policy.Rules {
			if policy.Rules[i].Name == entry.RuleName {
				policy.Rules = append(policy.Rules[:i], policy.Rules[i+1:]...)
				break
			}
		}

		// Unmerge the rules that were merged with this rule
		for _, mergeeRuleName := range entry.OtherRuleNames {
			mergeeRuleEntry := mergedRulesMap[rsName][mergeeRuleName]
			mergeeRule := mergeeRuleEntry.OriginalRule

			// Add unmerged rule back to policy delete its mergedRulesMap entry
			policy.Rules = append(policy.Rules, mergeeRule)
			delete(mergedRulesMap[rsName], mergeeRuleName)
		}

		// Reset the policy and delete this mergedRulesMap entry and then merge the rules of the reset policy
		rc.SetPolicy(*policy)
		delete(mergedRulesMap[rsName], ruleName)
		rc.MergeRules(mergedRulesMap)
		// This rule was merged into anther rule
	} else {
		mergerRuleName := entry.OtherRuleNames[0]
		mergerRuleEntry := mergedRulesMap[rsName][mergerRuleName]

		// Remove ruleName from merged rule entry
		for i := range mergerRuleEntry.OtherRuleNames {
			if mergerRuleEntry.OtherRuleNames[i] == ruleName {
				mergerRuleEntry.OtherRuleNames = append(mergerRuleEntry.OtherRuleNames[:i], mergerRuleEntry.OtherRuleNames[i+1:]...)
				break
			}
		}

		// Get the merged actions and delete them from the merger entry and delete mergedRulesMap entries
		mergedActions := mergerRuleEntry.MergedActions[ruleName]
		delete(mergerRuleEntry.MergedActions, ruleName)
		if len(mergerRuleEntry.MergedActions) == 0 {
			delete(mergedRulesMap[rsName], mergerRuleName)
		}
		delete(mergedRulesMap[rsName], ruleName)

		// Find the merger rule and delete the merged actions from it
		policy := rc.FindPolicy("forwarding")
		for i := range policy.Rules {
			if policy.Rules[i].Name == mergerRuleName {
				var deletedActionIndices []int

				// Find and these merged actions indices
				for j := range mergedActions {
					for k := range policy.Rules[i].Actions {
						if mergedActions[j] == policy.Rules[i].Actions[k] {
							deletedActionIndices = append(deletedActionIndices, k)
						}
					}
				}

				// Delete these merged actions
				for _, index := range deletedActionIndices {
					policy.Rules[i].Actions = append(policy.Rules[i].Actions[:index], policy.Rules[i].Actions[index+1:]...)
					for j := range deletedActionIndices {
						deletedActionIndices[j]--
					}
				}

				// Delete the merged rule if everything has been unmerged
				if len(policy.Rules[i].Actions) == 0 {
					policy.Rules = append(policy.Rules[:i], policy.Rules[i+1:]...)
				}
				break
			}
		}

		// Delete the policy if its rules are empty
		if len(policy.Rules) == 0 {
			rc.RemovePolicy(*policy)
		} else {
			rc.SetPolicy(*policy)
		}
	}

	// Delete entry from the mergedRulesMap if it is empty for this config
	if len(mergedRulesMap[rsName]) == 0 {
		delete(mergedRulesMap, rsName)
	}
	return true
}

func (cfg *ResourceConfig) GetName() string {
	return cfg.Virtual.Name
}

func (rc *ResourceConfig) MergeRules(mergedRulesMap map[string]map[string]mergedRuleEntry) {
	policy := rc.FindPolicy("forwarding")
	if policy == nil {
		return
	}

	rules := policy.Rules

	var iDeletedRuleIndices []int
	var jDeletedRuleIndices []int

	// Iterate through the rules and compare them to each other
	for i, rl := range rules {
		if strings.HasSuffix(rl.Name, "-reset") {
			continue
		}
		// Do not merge the same rule to itself or to rules that have already been merged
		for j := i + 1; j < len(rules); j++ {
			if strings.HasSuffix(rules[j].Name, "-reset") {
				continue
			}
			numMatches := 0
			numIConditions := len(rules[i].Conditions)
			numJConditions := len(rules[j].Conditions)
			if numIConditions == numJConditions {
				for k := range rules[i].Conditions {
					for l := range rules[j].Conditions {
						kConditionName := rules[i].Conditions[k].Name
						lConditionName := rules[j].Conditions[l].Name
						rules[i].Conditions[k].Name = ""
						rules[j].Conditions[l].Name = ""
						if reflect.DeepEqual(rules[i].Conditions[k], rules[j].Conditions[l]) {
							numMatches++
						}
						rules[i].Conditions[k].Name = kConditionName
						rules[j].Conditions[l].Name = lConditionName
					}
				}

				// Only merge if both sets of conditions match
				if numMatches == numIConditions {
					var mergerEntry mergedRuleEntry
					var mergeeEntry mergedRuleEntry

					iName := rules[i].Name
					jName := rules[j].Name
					// Merge rule[i] into rule[j]
					if ((strings.Contains(iName, "app-root") || strings.Contains(iName, "url-rewrite")) && !(strings.Contains(jName, "app-root") || strings.Contains(jName, "url-rewrite"))) ||
						((strings.Contains(iName, "app-root") || strings.Contains(iName, "url-rewrite")) && (strings.Contains(jName, "app-root") || strings.Contains(jName, "url-rewrite"))) {
						iDeletedRuleIndices = append(iDeletedRuleIndices, i)
						mergerEntry.RuleName = jName
						mergeeEntry.RuleName = iName
						mergerEntry.OtherRuleNames = []string{iName}
						mergeeEntry.OtherRuleNames = []string{jName}
						mergerEntry.OriginalRule = rules[j]
						mergeeEntry.OriginalRule = rules[i]

						// Merge only unique actions
						for k := range rules[i].Actions {
							found := false
							for l := range rules[j].Actions {
								mergeeName := rules[i].Actions[k].Name
								mergerName := rules[j].Actions[l].Name
								rules[i].Actions[k].Name = ""
								rules[j].Actions[l].Name = ""
								if reflect.DeepEqual(rules[i].Actions[k], rules[j].Actions[l]) {
									found = true
								}
								rules[i].Actions[k].Name = mergeeName
								rules[j].Actions[l].Name = mergerName
							}
							if !found {
								rules[j].Actions = append(rules[j].Actions, rules[i].Actions[k])
								mergerEntry.MergedActions = make(map[string][]*action)
								mergerEntry.MergedActions[iName] = append(mergerEntry.MergedActions[iName], rules[i].Actions[k])
							}
						}
						// Merge rule[j] into rule[i]
					} else if !(strings.Contains(iName, "app-root") || strings.Contains(iName, "url-rewrite")) && (strings.Contains(jName, "app-root") || strings.Contains(jName, "url-rewrite")) {
						jDeletedRuleIndices = append(jDeletedRuleIndices, j)
						mergerEntry.RuleName = iName
						mergeeEntry.RuleName = jName
						mergerEntry.OtherRuleNames = []string{jName}
						mergeeEntry.OtherRuleNames = []string{iName}
						mergerEntry.OriginalRule = rules[i]
						mergeeEntry.OriginalRule = rules[j]

						// Merge only unique actions
						for k := range rules[j].Actions {
							found := false
							for l := range rules[i].Actions {
								mergeeName := rules[j].Actions[k].Name
								mergerName := rules[i].Actions[l].Name
								rules[j].Actions[k].Name = ""
								rules[i].Actions[l].Name = ""
								if reflect.DeepEqual(rules[j].Actions[k], rules[i].Actions[l]) {
									found = true
								}
								rules[j].Actions[k].Name = mergeeName
								rules[i].Actions[l].Name = mergerName
							}
							if !found {
								rules[i].Actions = append(rules[i].Actions, rules[j].Actions[k])
								mergerEntry.MergedActions = make(map[string][]*action)
								mergerEntry.MergedActions[jName] = append(mergerEntry.MergedActions[jName], rules[j].Actions[k])
							}
						}
					}

					contains := func(slice []string, s string) bool {
						for _, v := range slice {
							if v == s {
								return true
							}
						}
						return false
					}

					// Process entries to the mergedRulesMap
					key := rc.GetName()
					if len(mergerEntry.MergedActions) != 0 {
						// Check if there is are entries for this resource config
						if _, ok := mergedRulesMap[key]; ok {
							// See if there is an entry for the merger
							if entry, ok := mergedRulesMap[key][mergerEntry.RuleName]; ok {
								if !contains(entry.OtherRuleNames, mergerEntry.OtherRuleNames[0]) {
									mergerEntry.OtherRuleNames = append(mergerEntry.OtherRuleNames, entry.OtherRuleNames...)
								}
								mergerEntry.OriginalRule = entry.OriginalRule

								if len(entry.MergedActions) != 0 {
									for k, v := range entry.MergedActions {
										mergerEntry.MergedActions[k] = v
									}
								}
							}
							// See if there is an entry for the mergee
							if entry, ok := mergedRulesMap[key][mergeeEntry.RuleName]; ok {
								mergeeEntry.OriginalRule = entry.OriginalRule
							}
						} else {
							mergedRulesMap[key] = make(map[string]mergedRuleEntry)
						}

						mergedRulesMap[key][mergerEntry.RuleName] = mergerEntry
						mergedRulesMap[key][mergeeEntry.RuleName] = mergeeEntry
					}
				}
			}
		}
	}

	// Process deleted rule indices and remove duplicates
	deletedRuleIndices := append(iDeletedRuleIndices, jDeletedRuleIndices...)
	sort.Ints(deletedRuleIndices)
	var uniqueDeletedRuleIndices []int
	for i := range deletedRuleIndices {
		if i == 0 {
			uniqueDeletedRuleIndices = append(uniqueDeletedRuleIndices, deletedRuleIndices[i])
		} else {
			found := false
			for j := range uniqueDeletedRuleIndices {
				if uniqueDeletedRuleIndices[j] == deletedRuleIndices[i] {
					found = true
				}
			}
			if !found {
				uniqueDeletedRuleIndices = append(uniqueDeletedRuleIndices, deletedRuleIndices[i])
			}
		}
	}

	// Remove rules that were merged with others
	for _, index := range uniqueDeletedRuleIndices {
		rules = append(rules[:index], rules[index+1:]...)
		for i := range uniqueDeletedRuleIndices {
			uniqueDeletedRuleIndices[i]--
		}
	}

	// Sort the rules
	//sort.Sort(sort.Reverse(&rules))

	policy.Rules = rules
	rc.SetPolicy(*policy)
}

func (rcs ResourceConfigs) GetAllPoolMembers() []Member {
	// Get all pool members and write them to VxlanMgr to configure ARP entries
	var allPoolMembers []Member

	for _, cfg := range rcs {
		// Filter the configs to only those that have active services
		if cfg.MetaData.Active {
			for _, pool := range cfg.Pools {
				allPoolMembers = append(allPoolMembers, pool.Members...)
			}
		}
	}
	return allPoolMembers
}

func (rs *Resources) updateOldConfig() {
	rs.oldRsMap = make(ResourceConfigMap)
	for k, v := range rs.rsMap {
		rs.oldRsMap[k] = &ResourceConfig{}
		rs.oldRsMap[k].copyConfig(v)
	}
}

// Deletes respective VirtualServer resource configuration from
// resource configs.
func (rs *Resources) deleteVirtualServer(rsName string) {
	delete(rs.rsMap, rsName)
}

func NewInternalDataGroup(name, partition string) *InternalDataGroup {
	// Need to explicitly initialize Records to an empty array so it isn't nil.
	return &InternalDataGroup{
		Name:      name,
		Partition: partition,
		Records:   []InternalDataGroupRecord{},
	}
}

// DataGroup flattening.
type FlattenConflictFunc func(key, oldVal, newVal string) string

// Internal data group for passthrough routes to map server names to pools.
const PassthroughHostsDgName = "ssl_passthrough_servername_dg"

// Internal data group for reencrypt routes.
const ReencryptHostsDgName = "ssl_reencrypt_servername_dg"

// Internal data group for edge routes.
const EdgeHostsDgName = "ssl_edge_servername_dg"

// Internal data group for reencrypt routes that maps the host name to the
// server ssl profile.
const ReencryptServerSslDgName = "ssl_reencrypt_serverssl_dg"

// Internal data group for edge routes that maps the host name to the
// false. This will help Irule to understand ssl should be disabled
// on serverside.
const EdgeServerSslDgName = "ssl_edge_serverssl_dg"

// Internal data group for ab deployment routes.
const AbDeploymentDgName = "ab_deployment_dg"

var groupFlattenFuncMap = map[string]FlattenConflictFunc{
	PassthroughHostsDgName:   flattenConflictWarn,
	ReencryptHostsDgName:     flattenConflictWarn,
	EdgeHostsDgName:          flattenConflictWarn,
	ReencryptServerSslDgName: flattenConflictWarn,
	EdgeServerSslDgName:      flattenConflictWarn,
	HttpsRedirectDgName:      flattenConflictConcat,
	AbDeploymentDgName:       flattenConflictConcat,
}

func flattenConflictConcat(key, oldVal, newVal string) string {
	// Tokenize both values and add to a map to ensure uniqueness
	pathMap := make(map[string]bool)
	for _, token := range strings.Split(oldVal, "|") {
		pathMap[token] = true
	}
	for _, token := range strings.Split(newVal, "|") {
		pathMap[token] = true
	}

	// Convert back to an array
	paths := []string{}
	for path, _ := range pathMap {
		paths = append(paths, path)
	}

	// Sort the paths to have consistent ordering
	sort.Strings(paths)

	// Write back out to a delimited string
	var buf bytes.Buffer
	for i, path := range paths {
		if i > 0 {
			buf.WriteString("|")
		}
		buf.WriteString(path)
	}

	return buf.String()
}

func flattenConflictWarn(key, oldVal, newVal string) string {
	fmt.Printf("Found mismatch for key '%v' old value: '%v' new value: '%v'\n", key, oldVal, newVal)
	return oldVal
}

func (dgnm DataGroupNamespaceMap) FlattenNamespaces() *InternalDataGroup {

	// Try to be efficient in these common cases.
	if len(dgnm) == 0 {
		// No namespaces.
		return nil
	} else if len(dgnm) == 1 {
		// Only 1 namespace, just return its dg - no flattening needed.
		for _, dg := range dgnm {
			return dg
		}
	}

	// Use a map to identify duplicates across namespaces
	var partition, name string
	flatMap := make(map[string]string)
	for _, dg := range dgnm {
		if partition == "" {
			partition = dg.Partition
		}
		if name == "" {
			name = dg.Name
		}
		for _, rec := range dg.Records {
			item, found := flatMap[rec.Name]
			if found {
				if item != rec.Data {
					conflictFunc, ok := groupFlattenFuncMap[dg.Name]
					if !ok {
						log.Warningf("[RESOURCE] No DataGroup conflict handler defined for '%v'",
							dg.Name)
						conflictFunc = flattenConflictWarn
					}
					newVal := conflictFunc(rec.Name, item, rec.Data)
					flatMap[rec.Name] = newVal
				}
			} else {
				flatMap[rec.Name] = rec.Data
			}
		}
	}

	// Create a new datagroup to hold the flattened results
	newDg := InternalDataGroup{
		Partition: partition,
		Name:      name,
	}
	for name, data := range flatMap {
		newDg.AddOrUpdateRecord(name, data)
	}

	return &newDg
}

func (slice InternalDataGroupRecords) Less(i, j int) bool {
	return slice[i].Name < slice[j].Name
}

func (slice InternalDataGroupRecords) Len() int {
	return len(slice)
}

func (slice InternalDataGroupRecords) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

func (idg *InternalDataGroup) AddOrUpdateRecord(name, data string) bool {
	// The records are maintained as a sorted array.
	nameKeyFunc := func(i int) bool {
		return idg.Records[i].Name >= name
	}
	i := sort.Search(idg.Records.Len(), nameKeyFunc)
	if i < idg.Records.Len() && idg.Records[i].Name == name {
		if idg.Records[i].Data != data {
			// name found with different data, update
			idg.Records[i].Data = data
			return true
		}
		// name found with same data
		return false
	}

	// Insert into the correct position.
	idg.Records = append(idg.Records, InternalDataGroupRecord{})
	copy(idg.Records[i+1:], idg.Records[i:])
	idg.Records[i] = InternalDataGroupRecord{Name: name, Data: data}

	return true
}

func (idg *InternalDataGroup) RemoveRecord(name string) bool {
	// The records are maintained as a sorted array.
	nameKeyFunc := func(i int) bool {
		return idg.Records[i].Name >= name
	}
	nbrRecs := idg.Records.Len()
	i := sort.Search(nbrRecs, nameKeyFunc)
	if i < nbrRecs && idg.Records[i].Name == name {
		// found, remove it and adjust the array.
		nbrRecs -= 1
		copy(idg.Records[i:], idg.Records[i+1:])
		idg.Records[nbrRecs] = InternalDataGroupRecord{}
		idg.Records = idg.Records[:nbrRecs]
		return true
	}
	return false
}

// AS3NameFormatter formarts resources names according to AS3 convention
// TODO: Should we use this? Or this will be done in agent?
func AS3NameFormatter(name string) string {
	replacer := strings.NewReplacer(".", "_", ":", "_", "/", "_", "%", ".", "-", "_", "=", "_")
	name = replacer.Replace(name)
	return name
}
