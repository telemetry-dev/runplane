package sysbox

import (
	"context"
	"fmt"
	"net/netip"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const (
	networkName = "runplane"
	bridgeName  = "runplane0"
)

var defaultSubnet = netip.MustParsePrefix("10.213.0.0/16")

// bridgeNet is the runplane network as the firewall rules see it.
type bridgeNet struct {
	bridge  string
	subnet  netip.Prefix
	gateway netip.Addr // also the service address of the sandboxes
}

// ensureNetwork makes the Docker network "runplane", or reads the one that
// runplane made before. Traffic between its containers is off.
func (d *Driver) ensureNetwork(ctx context.Context) (bridgeNet, error) {
	res, err := d.docker.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		subnet := defaultSubnet
		gateway := subnet.Addr().Next()
		_, err = d.docker.NetworkCreate(ctx, networkName, client.NetworkCreateOptions{
			Driver: "bridge",
			IPAM:   &network.IPAM{Config: []network.IPAMConfig{{Subnet: subnet, Gateway: gateway}}},
			Options: map[string]string{
				"com.docker.network.bridge.name":       bridgeName,
				"com.docker.network.bridge.enable_icc": "false",
			},
			Labels: map[string]string{labelManaged: "true"},
		})
		if err != nil && !cerrdefs.IsConflict(err) {
			return bridgeNet{}, fmt.Errorf("make network %s: %w", networkName, err)
		}
		d.log.Info("made the sandbox network", "network", networkName, "subnet", subnet)
		res, err = d.docker.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{})
	}
	if err != nil {
		return bridgeNet{}, fmt.Errorf("inspect network %s: %w", networkName, err)
	}
	return readNetwork(res.Network.Network)
}

func readNetwork(n network.Network) (bridgeNet, error) {
	if n.Labels[labelManaged] != "true" || n.Driver != "bridge" {
		return bridgeNet{}, fmt.Errorf("the Docker network %s is not a bridge that runplane made; remove it or rename it", networkName)
	}
	if n.Options["com.docker.network.bridge.enable_icc"] != "false" {
		return bridgeNet{}, fmt.Errorf("the Docker network %s lets containers talk to each other; remove it, and runplane makes it again", networkName)
	}
	br := bridgeNet{bridge: n.Options["com.docker.network.bridge.name"]}
	if br.bridge == "" {
		br.bridge = "br-" + n.ID[:12]
	}
	for _, c := range n.IPAM.Config {
		if c.Subnet.Addr().Is4() && c.Gateway.Is4() {
			br.subnet, br.gateway = c.Subnet.Masked(), c.Gateway
			return br, nil
		}
	}
	return bridgeNet{}, fmt.Errorf("the Docker network %s has no IPv4 subnet with a gateway", networkName)
}
