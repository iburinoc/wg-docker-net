package wg

import (
	"fmt"
	"log"

	"github.com/davecgh/go-spew/spew"
	"github.com/docker/go-plugins-helpers/network"
	"github.com/vishvananda/netns"
)

type Driver struct {
	networks map[string]*Network
	rootNs   netns.NsHandle
	iptables *Iptables
}

func notSupported(method string) error {
	return fmt.Errorf("[%v] not supported", method)
}

func logRequest(method string, request interface{}) {
	str := spew.Sdump(request)
	log.Printf("[%s] request: %s\n", method, str)
}

func NewDriver() (*Driver, error) {
	rootNs, err := netns.GetFromPid(1)
	if err != nil {
		return nil, fmt.Errorf("Error getting root namespace: %v", err)
	}
	log.Printf("Got root namespace at fd %d\n", rootNs)

	iptables, err := CreateIptables()
	if err != nil {
		return nil, err
	}

	return &Driver{
		networks: make(map[string]*Network),
		rootNs:   rootNs,
		iptables: iptables,
	}, nil
}

func (t *Driver) Delete() error {
	errs := make([]error, 0)
	for _, net := range t.networks {
		if err := net.Delete(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := t.iptables.Delete(t.rootNs); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("Failed to cleanup: %v", errs)
	}
	return nil
}

func (t *Driver) GetCapabilities() (*network.CapabilitiesResponse, error) {
	logRequest("GetCapabilities", nil)

	response := &network.CapabilitiesResponse{
		Scope:             network.LocalScope,
		ConnectivityScope: network.LocalScope,
	}
	log.Printf("[GetCapabilities] response: %+v\n", response)
	return response, nil
}

func (t *Driver) CreateNetwork(req *network.CreateNetworkRequest) error {
	logRequest("CreateNetwork", req)

	if len(req.IPv4Data) > 1 || len(req.IPv6Data) > 0 {
		return fmt.Errorf("Multiple ipv4 data or ipv6 data not supported")
	}

	options := req.Options["com.docker.network.generic"].(map[string]interface{})
	network, err := CreateNetwork(req.IPv4Data[0], options, t.rootNs, t.iptables)
	if err != nil {
		return err
	}
	t.networks[req.NetworkID] = network

	return nil
}

func (t *Driver) DeleteNetwork(req *network.DeleteNetworkRequest) error {
	logRequest("DeleteNetwork", req)

	id := req.NetworkID
	net := t.networks[id]
	if net == nil {
		return fmt.Errorf("Network %s not found\n", id)
	}
	delete(t.networks, id)

	return net.Delete()
}

func (t *Driver) AllocateNetwork(req *network.AllocateNetworkRequest) (*network.AllocateNetworkResponse, error) {
	logRequest("AllocateNetwork", req)
	return nil, notSupported("AllocateNetwork")
}

func (t *Driver) FreeNetwork(req *network.FreeNetworkRequest) error {
	logRequest("FreeNetwork", req)
	return notSupported("FreeNetwork")
}

func (t *Driver) CreateEndpoint(req *network.CreateEndpointRequest) (*network.CreateEndpointResponse, error) {
	logRequest("CreateEndpoint", req)

	net := t.networks[req.NetworkID]
	if net == nil {
		return nil, fmt.Errorf("Network %s not found", req.NetworkID)
	}

	intf, err := net.CreateEndpoint(req.EndpointID, req.Interface)
	if err != nil {
		return nil, err
	}
	if req.Interface.Address == intf.Address {
		intf.Address = ""
	}
	if req.Interface.MacAddress == intf.MacAddress {
		intf.MacAddress = ""
	}
	return &network.CreateEndpointResponse{intf}, nil
}

func (t *Driver) DeleteEndpoint(req *network.DeleteEndpointRequest) error {
	logRequest("DeleteEndpoint", req)

	net := t.networks[req.NetworkID]
	if net == nil {
		return fmt.Errorf("Network %s not found", req.NetworkID)
	}
	return net.DeleteEndpoint(req.EndpointID)
}

func (t *Driver) EndpointInfo(req *network.InfoRequest) (*network.InfoResponse, error) {
	logRequest("EndpointInfo", req)
	return &network.InfoResponse{Value: make(map[string]string, 0)}, nil
}

func (t *Driver) Join(req *network.JoinRequest) (*network.JoinResponse, error) {
	logRequest("Join", req)

	net := t.networks[req.NetworkID]
	if net == nil {
		return nil, fmt.Errorf("Network %s not found", req.NetworkID)
	}
	response, err := net.Join(req.EndpointID)
	return response, err
}

func (t *Driver) Leave(req *network.LeaveRequest) error {
	logRequest("Leave", req)

	net := t.networks[req.NetworkID]
	if net == nil {
		return fmt.Errorf("Network %s not found", req.NetworkID)
	}
	return net.Leave(req.EndpointID)
}

func (t *Driver) DiscoverNew(req *network.DiscoveryNotification) error {
	logRequest("DiscoverNew", req)
	return notSupported("DiscoverNew")
}

func (t *Driver) DiscoverDelete(req *network.DiscoveryNotification) error {
	logRequest("DiscoverDelete", req)
	return notSupported("DiscoverDelete")
}

func (t *Driver) ProgramExternalConnectivity(req *network.ProgramExternalConnectivityRequest) error {
	logRequest("ProgramExternalConnectivity", req)
	return notSupported("ProgramExternalConnectivity")
}

func (t *Driver) RevokeExternalConnectivity(req *network.RevokeExternalConnectivityRequest) error {
	logRequest("RevokeExternalConnectivity", req)
	return notSupported("RevokeExternalConnectivity")
}
