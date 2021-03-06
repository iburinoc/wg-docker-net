package wg

import (
	"crypto/rand"
	"net"

	"github.com/docker/go-plugins-helpers/network"
)

type Endpoint struct {
	Addr *net.IPNet
	Mac  net.HardwareAddr
}

func CreateEndpoint(intf *network.EndpointInterface, ipAllocator *IpAllocator) (*Endpoint, error) {
	var addr *net.IPNet
	var mac net.HardwareAddr
	var err error

	if intf.Address != "" {
		var ipAddr net.IP
		ipAddr, addr, err = net.ParseCIDR(intf.Address)
		if err != nil {
			return nil, err
		}
		addr.IP = ipAddr
	} else {
		addr, err = ipAllocator.FindAddress()
		if err != nil {
			return nil, err
		}
	}

	if intf.MacAddress != "" {
		mac, err = net.ParseMAC(intf.MacAddress)
		if err != nil {
			return nil, err
		}
	} else {
		mac = make(net.HardwareAddr, 6)
		_, err = rand.Read(mac)
		if err != nil {
			return nil, err
		}
		mac[0] = (mac[0] & 0xfe) | 0x02
	}

	return &Endpoint{addr, mac}, nil
}

func (t *Endpoint) CreateEndpointResponse() *network.EndpointInterface {
	return &network.EndpointInterface{
		Address:    t.Addr.String(),
		MacAddress: t.Mac.String(),
	}
}
