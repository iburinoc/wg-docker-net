package wg

import (
	"fmt"
	"log"

	"github.com/davecgh/go-spew/spew"
	"github.com/docker/go-plugins-helpers/network"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	LINK_PREFIX = "wgdocknet"
)

type Network struct {
	ns     netns.NsHandle
	nl     *netlink.Handle
	rootNs netns.NsHandle
	rootNl *netlink.Handle
	name   *string
	conf   *WgConfig
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

func CreateNetwork(data *network.IPAMData, options map[string]interface{}, rootNs netns.NsHandle) (*Network, error) {
	var ns netns.NsHandle
	var err error

	confPath := getOpt(options, "wg.wgconf")

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

	name := getOpt(options, "wg.namespace")
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
		if err != nil {
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

	// TODO: Add a defer to bring this down if needed
	err = createOutboundLink(ns, rootNs, rootNl)
	if err != nil {
		return nil, err
	}

	// TODO: Add a defer to bring this down if needed
	err = conf.StartInterface()
	if err != nil {
		return nil, err
	}

	return &Network{
		ns,
		nl,
		rootNs,
		rootNl,
		name,
		conf,
	}, nil
}

func (t *Network) Delete() error {
	t.nl.Delete()

	err := deleteNs(t.ns, t.name)
	return err
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

func findUnusedLinkName(nsHandle *netlink.Handle) (string, error) {
	names, err := allLinkNames(nsHandle)
	if err != nil {
		return "", err
	}

	nameSet := make(map[string]struct{})
	for _, name := range names {
		nameSet[name] = struct{}{}
	}

	for i := 0; true; i++ {
		possibleName := fmt.Sprintf("%s%d", LINK_PREFIX, i)

		_, exists := nameSet[possibleName]
		if !exists {
			return possibleName, nil
		}
	}

	return "", fmt.Errorf("Impossible")
}

func createOutboundLink(ns, rootNs netns.NsHandle, rootNl *netlink.Handle) error {
	publicName, err := findUnusedLinkName(rootNl)
	if err != nil {
		return err
	}

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name:      "veth0",
			Namespace: ns,
		},
		PeerName: publicName,
	}

	return rootNl.LinkAdd(veth)
}
