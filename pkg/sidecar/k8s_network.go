//go:build linux
// +build linux

package sidecar

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/testground/sdk-go/network"
	"github.com/testground/testground/pkg/docker"
	"github.com/testground/testground/pkg/logging"

	"github.com/containernetworking/cni/libcni"
	"github.com/vishvananda/netlink"
)

type k8sLink struct {
	*NetlinkLink
	IPv4, IPv6 *net.IPNet

	rt      *libcni.RuntimeConf
	netconf *libcni.NetworkConfigList
}

type K8sNetwork struct {
	container       *docker.ContainerRef
	activeLinks     map[string]*k8sLink
	externalRouting map[string]*route
	nl              *netlink.Handle
	cninet          *libcni.CNIConfig
	subnet          string
	netnsPath       string
}

func (n *K8sNetwork) Close() error {
	n.nl.Delete()
	return nil
}

func (n *K8sNetwork) ConfigureNetwork(ctx context.Context, cfg *network.Config) error {
	if cfg.Network != defaultDataNetwork {
		return fmt.Errorf("configured network is not `%s`", defaultDataNetwork)
	}

	var skipConfig = true
	if skipConfig {
		logging.S().Debug("Skipping network configuration completely!")
		return nil
	}

	logging.S().Debugw("============ Configuring network START ==============", "network", cfg.Network)

	for k, v := range n.activeLinks {
		logging.S().Debugf("Active link %s: %s (ipv4) %s (link.Type) %s (rt.IfName) %s (rt.NetNS) %s (netconf.Name)\n",
			k, v.IPv4, v.Link.Type(), v.rt.IfName, v.rt.NetNS, v.netconf.Name)
	}

	link, online := n.activeLinks[cfg.Network]

	// Are we _disabling_ the network?
	if !cfg.Enable {
		// Yes, is it already disabled?
		if online {
			// No. Disconnect.
			if err := n.cninet.DelNetworkList(ctx, link.netconf, link.rt); err != nil {
				return fmt.Errorf("when 6: %w", err)
			}
			delete(n.activeLinks, cfg.Network)
		}
		return nil
	}

	if online && ((cfg.IPv6 != nil && !link.IPv6.IP.Equal(cfg.IPv6.IP)) ||
		(cfg.IPv4 != nil && !link.IPv4.IP.Equal(cfg.IPv4.IP))) {
		// Disconnect and reconnect to change the IP addresses.
		logging.S().Debugw("disconnect and reconnect to change the IP addr", "cfg.IPv4", cfg.IPv4, "link.IPv4", link.IPv4.String(), "container", n.container.ID)
		//
		// NOTE: We probably don't need to do this on local docker.
		// However, we probably do with swarm.
		online = false
		if err := n.cninet.DelNetworkList(ctx, link.netconf, link.rt); err != nil {
			return fmt.Errorf("when 5: %w", err)
		}
		delete(n.activeLinks, cfg.Network)
	}

	// Are we _connected_ to the network.
	if !online {
		// No, we're not.
		// Connect.
		if cfg.IPv6 != nil {
			return errors.New("ipv6 not supported")
		}

		var (
			netconf *libcni.NetworkConfigList
			err     error
		)
		if cfg.IPv4 == nil {
			logging.S().Debugw("trying to add a link", "net", n.subnet, "container", n.container.ID)
			netconf, err = newNetworkConfigList("net", n.subnet)
		} else {
			logging.S().Debugw("trying to add a link", "ip", cfg.IPv4.String(), "container", n.container.ID)
			netconf, err = newNetworkConfigList("ip", cfg.IPv4.String())
		}
		if err != nil {
			return fmt.Errorf("failed to generate new network config list: %w", err)
		}

		cniArgs := [][2]string{}                   // empty
		capabilityArgs := map[string]interface{}{} // empty

		rt := &libcni.RuntimeConf{
			ContainerID:    n.container.ID,
			NetNS:          n.netnsPath,
			IfName:         dataNetworkIfname,
			Args:           cniArgs,
			CapabilityArgs: capabilityArgs,
		}

		errc := make(chan error)

		go func() {
			err = retry(3, 2*time.Second, func() error {
				_, err = n.cninet.AddNetworkList(ctx, netconf, rt)
				return err
			})
			errc <- err
		}()

		select {
		case err := <-errc:
			if err != nil {
				return fmt.Errorf("failed to add network through cni plugin: %w", err)
			}
		case <-time.After(30 * time.Second):
			return fmt.Errorf("timeout waiting on cninet.AddNetworkList")
		}

		netlinkByName, err := n.nl.LinkByName(dataNetworkIfname)
		if err != nil {
			return fmt.Errorf("failed to get link by name %s: %w", dataNetworkIfname, err)
		}

		routes, err := getK8sRoutes(netlinkByName, n.nl)
		for _, route := range routes.routes {
			logging.S().Debugw("Route in network:", "route", route)

		}
		logging.S().Debugf("External routing for network %s set to the routes logged above\n", dataNetworkIfname)
		n.externalRouting[dataNetworkIfname] = routes
		if err != nil {
			return err
		}

		// Register an active link.
		handle, err := NewNetlinkLink(n.nl, netlinkByName)
		if err != nil {
			return fmt.Errorf("failed to register new netlink: %w", err)
		}
		v4addrs, err := handle.ListV4()
		if err != nil {
			return fmt.Errorf("failed to list v4 addrs: %w", err)
		}

		logging.S().Debugf("Addresses in network %s are as follows:\n", dataNetworkIfname)
		for _, v4addr := range v4addrs {
			logging.S().Debugw("V4 addr", "address", v4addr)
		}

		if len(v4addrs) != 1 {
			logging.S().Warnf("Found %d v4 addresses, expected just 1", len(v4addrs))
			// return fmt.Errorf("expected 1 v4addrs, but received %d", len(v4addrs))
		}

		link = &k8sLink{
			NetlinkLink: handle,
			IPv4:        v4addrs[0],
			IPv6:        nil,
			rt:          rt,
			netconf:     netconf,
		}

		logging.S().Debugw("successfully adding an active link", "ipv4", link.IPv4, "container", n.container.ID)

		n.activeLinks[cfg.Network] = link
	}

	if err := link.Shape(cfg.Default); err != nil {
		return fmt.Errorf("failed to shape link: %w", err)
	}
	if err := link.AddRules(cfg.Rules); err != nil {
		return err
	}
	if err := handleRoutingPolicy(n.externalRouting, cfg.RoutingPolicy, n.nl); err != nil {
		return err
	}
	logging.S().Debugw("============ Configuring network END ==============", "network", cfg.Network)
	return nil
}

func (n *K8sNetwork) ListActive() []string {
	networks := make([]string, 0, len(n.activeLinks))
	for name := range n.activeLinks {
		networks = append(networks, name)
	}
	return networks
}

func newNetworkConfigList(t string, addr string) (*libcni.NetworkConfigList, error) {
	logging.S().Debugw("New network config list", t, addr)
	switch t {
	case "net":
		bytes := []byte(`
{
		"cniVersion": "0.3.0",
		"name": "weave-net",
		"plugins": [
				{
						"name": "weave-net",
						"type": "weave-net",
						"ipam": {
								"subnet": "` + addr + `"
						},
						"hairpinMode": true
				}
		]
}
`)
		return libcni.ConfListFromBytes(bytes)

	case "ip":
		bytes := []byte(`
{
		"cniVersion": "0.3.0",
		"name": "weave-net",
		"plugins": [
				{
						"name": "weave-net",
						"type": "weave-net",
						"ipam": {
								"ips": [
								  {
									  "version": "4",
										"address": "` + addr + `"
								  }
								]
						},
						"hairpinMode": true
				}
		]
}
`)
		return libcni.ConfListFromBytes(bytes)

	default:
		return nil, errors.New("unknown type")
	}
}

func getServiceRoute(handle *netlink.Handle, serviceIP net.IP) (*netlink.Route, error) {
	serviceRoutes, err := handle.RouteGet(serviceIP)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve route to service: %w", err)
	}

	if len(serviceRoutes) != 1 {
		return nil, fmt.Errorf("expected to get only one route to the given service, but got %v", len(serviceRoutes))
	}

	serviceRoute := serviceRoutes[0]

	return &serviceRoute, nil
}

func retry(attempts int, sleep time.Duration, f func() error) (err error) {
	for i := 0; ; i++ {
		err = f()
		if err == nil {
			return
		}

		logging.S().Warnw("got err, waiting to retry", "err", err.Error())

		if i >= (attempts - 1) {
			break
		}

		time.Sleep(sleep)
	}
	return fmt.Errorf("after %d attempts, last error: %s", attempts, err)
}
