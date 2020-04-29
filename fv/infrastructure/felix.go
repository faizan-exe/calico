// Copyright (c) 2017-2019 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package infrastructure

import (
	"fmt"
	"os"
	"path"
	"syscall"

	. "github.com/onsi/gomega"
	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/felix/fv/containers"
	"github.com/projectcalico/felix/fv/tcpdump"
	"github.com/projectcalico/felix/fv/utils"
)

type Felix struct {
	*containers.Container

	// ExpectedIPIPTunnelAddr contains the IP that the infrastructure expects to
	// get assigned to the IPIP tunnel.  Filled in by AddNode().
	ExpectedIPIPTunnelAddr string
	// ExpectedVXLANTunnelAddr contains the IP that the infrastructure expects to
	// get assigned to the VXLAN tunnel.  Filled in by AddNode().
	ExpectedVXLANTunnelAddr string

	// IP of the Typha that this Felix is using (if any).
	TyphaIP string

	startupDelayed bool
}

func (f *Felix) GetFelixPID() int {
	if f.startupDelayed {
		log.Panic("GetFelixPID() called but startup is delayed")
	}
	return f.GetSinglePID("calico-felix")
}

func (f *Felix) GetFelixPIDs() []int {
	if f.startupDelayed {
		log.Panic("GetFelixPIDs() called but startup is delayed")
	}
	return f.GetPIDs("calico-felix")
}

func (f *Felix) TriggerDelayedStart() {
	if !f.startupDelayed {
		log.Panic("TriggerDelayedStart() called but startup wasn't delayed")
	}
	f.Exec("touch", "/start-trigger")
	f.startupDelayed = false
}

func RunFelix(infra DatastoreInfra, id int, options TopologyOptions) *Felix {
	log.Info("Starting felix")
	ipv6Enabled := fmt.Sprint(options.EnableIPv6)

	args := infra.GetDockerArgs()
	args = append(args, "--privileged")

	// Add in the environment variables.
	envVars := map[string]string{
		"FELIX_LOGSEVERITYSCREEN":        options.FelixLogSeverity,
		"FELIX_PROMETHEUSMETRICSENABLED": "true",
		"FELIX_BPFLOGLEVEL":              "debug",
		"FELIX_USAGEREPORTINGENABLED":    "false",
		"FELIX_IPV6SUPPORT":              ipv6Enabled,
		// Disable log dropping, because it can cause flakes in tests that look for particular logs.
		"FELIX_DEBUGDISABLELOGDROPPING": "true",
	}

	containerName := containers.UniqueName(fmt.Sprintf("felix-%d", id))
	if os.Getenv("FELIX_FV_ENABLE_BPF") == "true" {
		envVars["FELIX_BPFENABLED"] = "true"

		// Disable map repinning by default since BPF map names are global and we don't want our simulated instances to
		// share maps.
		envVars["FELIX_DebugBPFMapRepinEnabled"] = "false"

		// FIXME: isolate individual Felix instances in their own cgroups.  Unfortunately, this doesn't work on systems that are using cgroupv1
		// see https://elixir.bootlin.com/linux/v5.3.11/source/include/linux/cgroup-defs.h#L788 for explanation.
		// envVars["FELIX_DEBUGBPFCGROUPV2"] = containerName
	}

	if options.DelayFelixStart {
		args = append(args, "-e", "DELAY_FELIX_START=true")
	}

	for k, v := range options.ExtraEnvVars {
		envVars[k] = v
	}

	for k, v := range envVars {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}

	// Add in the volumes.
	volumes := map[string]string{
		"/lib/modules": "/lib/modules",
	}
	for k, v := range options.ExtraVolumes {
		volumes[k] = v
	}
	for k, v := range volumes {
		args = append(args, "-v", fmt.Sprintf("%s:%s", k, v))
	}

	args = append(args,
		utils.Config.FelixImage,
	)

	c := containers.RunWithFixedName(containerName,
		containers.RunOpts{AutoRemove: true},
		args...,
	)

	if options.EnableIPv6 {
		c.Exec("sysctl", "-w", "net.ipv6.conf.all.disable_ipv6=0")
		c.Exec("sysctl", "-w", "net.ipv6.conf.default.disable_ipv6=0")
		c.Exec("sysctl", "-w", "net.ipv6.conf.lo.disable_ipv6=0")
		c.Exec("sysctl", "-w", "net.ipv6.conf.all.forwarding=1")
	} else {
		c.Exec("sysctl", "-w", "net.ipv6.conf.all.disable_ipv6=1")
		c.Exec("sysctl", "-w", "net.ipv6.conf.default.disable_ipv6=1")
		c.Exec("sysctl", "-w", "net.ipv6.conf.lo.disable_ipv6=1")
		c.Exec("sysctl", "-w", "net.ipv6.conf.all.forwarding=0")
	}

	// Configure our model host to drop forwarded traffic by default.  Modern
	// Kubernetes/Docker hosts now have this setting, and the consequence is that
	// whenever Calico policy intends to allow a packet, it must explicitly ACCEPT
	// that packet, not just allow it to pass through cali-FORWARD and assume it will
	// be accepted by the rest of the chain.  Establishing that setting in this FV
	// allows us to test that.
	c.Exec("iptables",
		"-w", "10", // Retry this for 10 seconds, e.g. if something else is holding the lock
		"-W", "100000", // How often to probe the lock in microsecs.
		"-P", "FORWARD", "DROP")

	return &Felix{
		Container:      c,
		startupDelayed: options.DelayFelixStart,
	}
}

func (f *Felix) Stop() {
	_ = f.ExecMayFail("rmdir", path.Join("/run/calico/cgroup/", f.Name))
	f.Container.Stop()
}

func (f *Felix) Restart() {
	oldPID := f.GetFelixPID()
	f.Signal(syscall.SIGHUP)
	Eventually(f.GetFelixPID, "10s", "100ms").ShouldNot(Equal(oldPID))
}

// AttachTCPDump returns tcpdump attached to the container
func (f *Felix) AttachTCPDump(iface string) *tcpdump.TCPDump {
	return tcpdump.Attach(f.Container.Name, "", iface)
}
