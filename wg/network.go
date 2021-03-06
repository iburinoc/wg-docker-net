package wg

import (
	"fmt"
	"log"
	"net"
	"strconv"

	"github.com/davecgh/go-spew/spew"
	"github.com/docker/go-plugins-helpers/network"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	LINK_PREFIX = "wgdocknet"
)

type Network struct {
	ns           netns.NsHandle
	nl           *netlink.Handle
	rootNs       netns.NsHandle
	rootNl       *netlink.Handle
	name         *string
	conf         *WgConfig
	bridge       *netlink.Bridge
	bridgeNet    *net.IPNet
	ipAllocator  *IpAllocator
	wgEndpoint   net.IP
	outboundAddr net.IP
	outboundIntf netlink.Link
	iptables     *Iptables

	endpoints  map[string]*Endpoint
	interfaces map[string]string
}

func getOpt(options map[string]interface{}, name string) *string {
	val, ok := options[name]
	if ok {
		str := val.(string)
		return &str
	} else {
		return nil
	}
}

func CreateNetwork(data *network.IPAMData, options map[string]interface{}, rootNs netns.NsHandle, iptables *Iptables) (*Network, error) {
	var ns netns.NsHandle
	var err error

	var doCleanup bool
	if val := getOpt(options, "cleanup"); val != nil {
		doCleanup, err = strconv.ParseBool(*val)
		if err != nil {
			return nil, err
		}
	} else {
		doCleanup = true
	}

	confPath := getOpt(options, "wgconf")

	wgEndpointAddr := getOpt(options, "endpoint")
	if wgEndpointAddr == nil {
		return nil, fmt.Errorf("No endpoint address provided")
	}

	wgEndpoint := net.ParseIP(*wgEndpointAddr)
	if wgEndpoint == nil {
		return nil, fmt.Errorf("Invalid endpoint address given: %s", *wgEndpointAddr)
	}

	rootNl, err := netlink.NewHandleAt(rootNs)
	if err != nil {
		return nil, fmt.Errorf("Error getting handle of root namespace: %v", err)
	}

	if confPath == nil {
		return nil, fmt.Errorf("Wireguard config file not present")
	}

	conf, err := ParseWgConfig(*confPath)
	if err != nil {
		return nil, err
	}
	str := spew.Sdump(*conf)
	log.Printf("Loaded wireguard config: %s\n", str)

	name := getOpt(options, "namespace")
	if name != nil {
		log.Printf("Creating namespace: %s\n", *name)
		ns, err = netns.NewNamed(*name)
		if err != nil {
			return nil, err
		}
	} else {
		log.Printf("Creating anonymous namespace\n")
		ns, err = netns.New()
		if err != nil {
			return nil, err
		}
	}
	defer func() {
		if err != nil && doCleanup {
			err = deleteNs(ns, name)
			if err != nil {
				log.Printf("Failed to cleanup namespace: %v\n", err)
			}
		}
	}()

	log.Printf("Created namespace at fd %d\n", ns)

	nl, err := netlink.NewHandleAt(ns)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			nl.Delete()
		}
	}()

	outboundAddr, outboundIntf, err := createOutboundLink(ns, rootNs, nl, rootNl)
	if err != nil {
		return nil, err
	}

	_, err = conf.StartInterface(nl)
	if err != nil {
		return nil, err
	}

	_, subnet, err := net.ParseCIDR(data.Pool)
	if err != nil {
		return nil, fmt.Errorf("Failed to parse assigned pool")
	}

	ipAllocator := CreateIpAllocator(subnet)
	ipAllocator.MarkUsed(conf.Net.IP)
	log.Printf("Marking wireguard link address used: %v", conf.Net.IP)

	bridgeNet, err := ipAllocator.FindAddress()
	if err != nil {
		return nil, fmt.Errorf("Failed to find address for bridge: %v", err)
	}

	bridge, err := createBridge(nl, bridgeNet)
	if err != nil {
		return nil, err
	}
	log.Printf("Created bridge with subnet: %v", bridgeNet)

	port := conf.ListenPort
	err = iptables.SetupForwarding(rootNs, outboundAddr, wgEndpoint, port)
	if err != nil {
		return nil, err
	}
	log.Printf("Setup iptables forwarding rules %v:%d <-> %v:%d", wgEndpoint, port, outboundAddr, port)

	endpoints := make(map[string]*Endpoint, 0)
	interfaces := make(map[string]string, 0)

	return &Network{
		ns,
		nl,
		rootNs,
		rootNl,
		name,
		conf,
		bridge,
		bridgeNet,
		ipAllocator,
		wgEndpoint,
		outboundAddr,
		outboundIntf,
		iptables,
		endpoints,
		interfaces,
	}, nil
}

func (t *Network) Delete() error {
	t.nl.Delete()

	err := deleteNs(t.ns, t.name)
	if err != nil {
		return err
	}

	if err = t.rootNl.LinkDel(t.outboundIntf); err != nil {
		return err
	}

	t.rootNl.Delete()

	err = t.iptables.RemoveForwarding(t.rootNs, t.outboundAddr, t.wgEndpoint, t.conf.ListenPort)
	return err
}

func (t *Network) CreateEndpoint(id string, intf *network.EndpointInterface) (*network.EndpointInterface, error) {
	if _, ok := t.endpoints[id]; ok {
		return nil, fmt.Errorf("Endpoint with this id already exists: %v", id)
	}

	endpoint, err := CreateEndpoint(intf, t.ipAllocator)
	if err != nil {
		return nil, err
	}

	t.endpoints[id] = endpoint

	response := endpoint.CreateEndpointResponse()

	log.Printf("Created endpoint with details: %v", response)

	return response, nil
}

func (t *Network) DeleteEndpoint(id string) error {
	endpoint, ok := t.endpoints[id]
	if !ok {
		return fmt.Errorf("Endpoint with this id not found: %v", id)
	}

	t.ipAllocator.MarkUnused(endpoint.Addr.IP)

	delete(t.endpoints, id)
	return nil
}

func (t *Network) Join(endpointId string) (*network.JoinResponse, error) {
	_, ok := t.endpoints[endpointId]
	if !ok {
		return nil, fmt.Errorf("Endpoint %s not found", endpointId)
	}

	publicLinkName, internalLinkName, err := createContainerLink(t.ns, t.rootNs, t.nl, t.rootNl, t.bridge)
	if err != nil {
		return nil, err
	}
	t.interfaces[endpointId] = internalLinkName

	routes := t.conf.GetRoutes(t.bridgeNet.IP)

	response := &network.JoinResponse{
		InterfaceName: network.InterfaceName{
			SrcName:   publicLinkName,
			DstPrefix: LINK_PREFIX,
		},
		StaticRoutes: routes,
	}

	str := spew.Sdump(*response)
	log.Printf("Responding to join request: %s\n", str)
	return response, nil
}

func (t *Network) Leave(endpointId string) error {
	interfaceName, ok := t.interfaces[endpointId]
	if !ok {
		return fmt.Errorf("Endpoint %s not found", endpointId)
	}
	delete(t.endpoints, endpointId)

	link, err := t.nl.LinkByName(interfaceName)
	if err != nil {
		return fmt.Errorf("Failed to delete interface, interface not found")
	}
	return t.nl.LinkDel(link)
}

func deleteNs(ns netns.NsHandle, name *string) error {
	if name != nil {
		err := netns.DeleteNamed(*name)
		if err != nil {
			return err
		}
	}

	err := ns.Close()
	return err
}

func allLinkNames(nsHandle *netlink.Handle) ([]string, error) {
	links, err := nsHandle.LinkList()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(links))
	for _, link := range links {
		names = append(names, link.Attrs().Name)
	}
	return names, nil
}

func findUnusedLinkName(prefix string, nsHandle *netlink.Handle) (string, error) {
	names, err := allLinkNames(nsHandle)
	if err != nil {
		return "", err
	}

	nameSet := make(map[string]struct{})
	for _, name := range names {
		nameSet[name] = struct{}{}
	}

	for i := 0; true; i++ {
		possibleName := fmt.Sprintf("%s%d", prefix, i)

		_, exists := nameSet[possibleName]
		if !exists {
			return possibleName, nil
		}
	}

	return "", fmt.Errorf("Impossible")
}

func allLinkNets(nsHandle *netlink.Handle) ([]net.IPNet, error) {
	addrs, err := nsHandle.AddrList(nil, 0)
	if err != nil {
		return nil, err
	}
	nets := make([]net.IPNet, 0)
	for _, addr := range addrs {
		nets = append(nets, *(addr.IPNet))
	}
	return nets, nil
}

func checkUnused(addr net.IP, used []net.IPNet) bool {
	for _, net := range used {
		if net.Contains(addr) {
			return false
		}
	}
	return true
}

// Use 17.31.X.X.  Maybe this should be configurable later but this is fine for now.
func findUnusedAddresses(nsHandle *netlink.Handle) (net.IP, net.IP, error) {
	nets, err := allLinkNets(nsHandle)
	if err != nil {
		return nil, nil, err
	}
	for i := 0; i < 65536; i += 2 {
		ip1 := net.IPv4(172, 31, byte(i/256), byte(i%256))
		ip2 := net.IPv4(172, 31, byte(i/256), byte((i%256)+1))

		if checkUnused(ip1, nets) && checkUnused(ip2, nets) {
			return ip1, ip2, nil
		}
	}
	return nil, nil, fmt.Errorf("Unable to find unused address")
}

func createOutboundLink(ns, rootNs netns.NsHandle, nl, rootNl *netlink.Handle) (net.IP, netlink.Link, error) {
	publicName, err := findUnusedLinkName(LINK_PREFIX, rootNl)
	if err != nil {
		return nil, nil, err
	}

	ip1, ip2, err := findUnusedAddresses(rootNl)
	if err != nil {
		return nil, nil, err
	}

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name:      publicName,
			Namespace: netlink.NsFd(rootNs),
		},
		PeerName: "veth0",
	}

	err = nl.LinkAdd(veth)
	if err != nil {
		return nil, nil, err
	}

	mask := net.CIDRMask(31, 32)

	outerAddr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ip1,
			Mask: mask,
		},
	}
	innerAddr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ip2,
			Mask: mask,
		},
	}
	err = rootNl.AddrAdd(veth, outerAddr)
	if err != nil {
		return nil, nil, err
	}
	err = rootNl.LinkSetUp(veth)
	if err != nil {
		return nil, nil, err
	}
	innerLink, err := nl.LinkByName("veth0")
	if err != nil {
		return nil, nil, err
	}
	err = nl.AddrAdd(innerLink, innerAddr)
	if err != nil {
		return nil, nil, err
	}
	err = nl.LinkSetUp(innerLink)
	if err != nil {
		return nil, nil, err
	}

	route := &netlink.Route{
		LinkIndex: innerLink.Attrs().Index,
		Dst: &net.IPNet{
			IP:   net.IPv4zero,
			Mask: net.CIDRMask(0, 32),
		},
		Src:   ip2,
		Gw:    ip1,
		Scope: netlink.SCOPE_UNIVERSE,
	}
	err = nl.RouteAdd(route)
	if err != nil {
		return nil, nil, fmt.Errorf("Error adding route: %v", err)
	}

	return ip2, veth, nil
}

func createContainerLink(ns, rootNs netns.NsHandle, nl, rootNl *netlink.Handle, bridge *netlink.Bridge) (string, string, error) {
	publicName, err := findUnusedLinkName(LINK_PREFIX, rootNl)
	if err != nil {
		return "", "", err
	}
	innerName, err := findUnusedLinkName("veth", nl)
	if err != nil {
		return "", "", err
	}

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name:      publicName,
			Namespace: netlink.NsFd(rootNs),
		},
		PeerName: innerName,
	}

	err = nl.LinkAdd(veth)
	if err != nil {
		return "", "", fmt.Errorf("Failed to create link (%s:%s) for container: %v", publicName, innerName, err)
	}

	err = rootNl.LinkSetUp(veth)
	if err != nil {
		return "", "", err
	}
	innerLink, err := nl.LinkByName(innerName)
	if err != nil {
		return "", "", err
	}
	err = nl.LinkSetMaster(innerLink, bridge)
	if err != nil {
		return "", "", err
	}
	err = nl.LinkSetUp(innerLink)
	if err != nil {
		return "", "", err
	}

	return publicName, innerName, nil
}

func createBridge(nl *netlink.Handle, net *net.IPNet) (*netlink.Bridge, error) {
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: "br0",
		},
	}

	err := nl.LinkAdd(bridge)
	if err != nil {
		return nil, fmt.Errorf("Failed to add bridge: %v", err)
	}

	addr := &netlink.Addr{
		IPNet: net,
	}
	err = nl.AddrAdd(bridge, addr)
	if err != nil {
		return nil, fmt.Errorf("Failed to set address for bridge: %v", err)
	}

	err = nl.LinkSetUp(bridge)
	if err != nil {
		return nil, fmt.Errorf("Failed to set bridge up: %v", err)
	}

	return bridge, nil
}
