package network

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/config"
)

func TestIPAMAllocatesAndReleases(t *testing.T) {
	_, cidr, err := net.ParseCIDR("172.31.0.0/29")
	require.NoError(t, err)
	p := newIPAM(cidr, net.ParseIP("172.31.0.1").To4())

	var got []string
	for i := 0; i < 5; i++ {
		ip, err := p.allocate()
		require.NoError(t, err)
		assert.NotEqual(t, "172.31.0.1", ip.String(), "gateway must not be handed out")
		got = append(got, ip.String())
	}
	assert.ElementsMatch(t, []string{"172.31.0.2", "172.31.0.3", "172.31.0.4", "172.31.0.5", "172.31.0.6"}, got)

	_, err = p.allocate()
	require.Error(t, err, "subnet is exhausted")

	p.release(net.ParseIP("172.31.0.2"))
	ip, err := p.allocate()
	require.NoError(t, err)
	assert.Equal(t, "172.31.0.2", ip.String())
}

func TestIPAMSkipsGatewayOnSmallSubnet(t *testing.T) {
	_, cidr, err := net.ParseCIDR("10.10.0.0/30")
	require.NoError(t, err)
	p := newIPAM(cidr, net.ParseIP("10.10.0.1").To4())
	// .0 network, .1 gateway, .2 usable, .3 broadcast.
	ip, err := p.allocate()
	require.NoError(t, err)
	assert.Equal(t, "10.10.0.2", ip.String())
	_, err = p.allocate()
	require.Error(t, err)
}

func TestNewValidatesConfiguration(t *testing.T) {
	_, err := New(config.NetworkConfig{
		Bridge:    "fcghact0",
		CIDR:      "172.31.0.0/16",
		Gateway:   "not-an-ip",
		MTU:       1500,
		TapPrefix: "fctap",
	})
	require.Error(t, err)

	m, err := New(config.NetworkConfig{
		Bridge:    "fcghact0",
		CIDR:      "172.31.0.0/16",
		Gateway:   "172.31.0.1",
		DNS:       []string{"1.1.1.1"},
		MTU:       1500,
		TapPrefix: "fctap",
	})
	require.NoError(t, err)
	assert.Len(t, m.DNS(), 1)
}
