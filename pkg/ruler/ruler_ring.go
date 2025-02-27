// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/ruler/ruler_ring.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package ruler

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/flagext"
	"github.com/grafana/dskit/kv"
	"github.com/grafana/dskit/netutil"
	"github.com/grafana/dskit/ring"
)

const (
	// If a ruler is unable to heartbeat the ring, its better to quickly remove it and resume
	// the evaluation of all rules since the worst case scenario is that some rulers will
	// receive duplicate/out-of-order sample errors.
	ringAutoForgetUnhealthyPeriods = 2
)

// RingOp is the operation used for distributing rule groups between rulers.
var RingOp = ring.NewOp([]ring.InstanceState{ring.ACTIVE}, func(s ring.InstanceState) bool {
	// Only ACTIVE rulers get any rule groups. If instance is not ACTIVE, we need to find another ruler.
	return s != ring.ACTIVE
})

// RingConfig masks the ring lifecycler config which contains
// many options not really required by the rulers ring. This config
// is used to strip down the config to the minimum, and avoid confusion
// to the user.
type RingConfig struct {
	KVStore          kv.Config     `yaml:"kvstore"`
	HeartbeatPeriod  time.Duration `yaml:"heartbeat_period" category:"advanced"`
	HeartbeatTimeout time.Duration `yaml:"heartbeat_timeout" category:"advanced"`

	// Instance details
	InstanceID             string   `yaml:"instance_id" doc:"default=<hostname>" category:"advanced"`
	InstanceInterfaceNames []string `yaml:"instance_interface_names" doc:"default=[<private network interfaces>]"`
	InstancePort           int      `yaml:"instance_port" category:"advanced"`
	InstanceAddr           string   `yaml:"instance_addr" category:"advanced"`
	NumTokens              int      `yaml:"num_tokens" category:"advanced"`

	// Injected internally
	ListenPort int `yaml:"-"`

	// Used for testing
	SkipUnregister bool `yaml:"-"`
}

// RegisterFlags adds the flags required to config this to the given FlagSet
func (cfg *RingConfig) RegisterFlags(f *flag.FlagSet, logger log.Logger) {
	hostname, err := os.Hostname()
	if err != nil {
		panic(fmt.Errorf("failed to get hostname, %w", err))
	}

	// Ring flags
	cfg.KVStore.Store = "memberlist" // Change default value to memberlist.
	cfg.KVStore.RegisterFlagsWithPrefix("ruler.ring.", "rulers/", f)
	f.DurationVar(&cfg.HeartbeatPeriod, "ruler.ring.heartbeat-period", 15*time.Second, "Period at which to heartbeat to the ring. 0 = disabled.")
	f.DurationVar(&cfg.HeartbeatTimeout, "ruler.ring.heartbeat-timeout", time.Minute, "The heartbeat timeout after which rulers are considered unhealthy within the ring. 0 = never (timeout disabled).")

	// Instance flags
	cfg.InstanceInterfaceNames = netutil.PrivateNetworkInterfacesWithFallback([]string{"eth0", "en0"}, logger)
	f.Var((*flagext.StringSlice)(&cfg.InstanceInterfaceNames), "ruler.ring.instance-interface-names", "List of network interface names to look up when finding the instance IP address.")
	f.StringVar(&cfg.InstanceAddr, "ruler.ring.instance-addr", "", "IP address to advertise in the ring. Default is auto-detected.")
	f.IntVar(&cfg.InstancePort, "ruler.ring.instance-port", 0, "Port to advertise in the ring (defaults to -server.grpc-listen-port).")
	f.StringVar(&cfg.InstanceID, "ruler.ring.instance-id", hostname, "Instance ID to register in the ring.")
	f.IntVar(&cfg.NumTokens, "ruler.ring.num-tokens", 128, "Number of tokens for each ruler.")
}

// ToLifecyclerConfig returns a LifecyclerConfig based on the ruler
// ring config.
func (cfg *RingConfig) ToLifecyclerConfig(logger log.Logger) (ring.BasicLifecyclerConfig, error) {
	instanceAddr, err := ring.GetInstanceAddr(cfg.InstanceAddr, cfg.InstanceInterfaceNames, logger)
	if err != nil {
		return ring.BasicLifecyclerConfig{}, err
	}

	instancePort := ring.GetInstancePort(cfg.InstancePort, cfg.ListenPort)

	return ring.BasicLifecyclerConfig{
		ID:                  cfg.InstanceID,
		Addr:                fmt.Sprintf("%s:%d", instanceAddr, instancePort),
		HeartbeatPeriod:     cfg.HeartbeatPeriod,
		HeartbeatTimeout:    cfg.HeartbeatTimeout,
		TokensObservePeriod: 0,
		NumTokens:           cfg.NumTokens,
	}, nil
}

func (cfg *RingConfig) ToRingConfig() ring.Config {
	rc := ring.Config{}
	flagext.DefaultValues(&rc)

	rc.KVStore = cfg.KVStore
	rc.HeartbeatTimeout = cfg.HeartbeatTimeout
	rc.SubringCacheDisabled = true

	// Each rule group is loaded to *exactly* one ruler.
	rc.ReplicationFactor = 1

	return rc
}
