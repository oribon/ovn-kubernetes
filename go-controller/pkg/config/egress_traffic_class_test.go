// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"github.com/urfave/cli/v2"
	"testing"
)

func TestTrafficClassRequiresEgressIP(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		env       string
		egressIP  bool
		enabled   bool
		wantError bool
	}{
		{name: "default"},
		{name: "missing EgressIP", args: []string{"--enable-traffic-class"}, wantError: true},
		{name: "explicitly disabled EgressIP", args: []string{"--enable-traffic-class", "--enable-egress-ip=false"}, wantError: true},
		{name: "flag", args: []string{"--enable-traffic-class", "--enable-egress-ip"}, egressIP: true, enabled: true},
		{name: "deployment environment", args: []string{"--enable-egress-ip"}, env: "true", egressIP: true, enabled: true},
		{name: "explicitly disabled", args: []string{"--enable-traffic-class=false"}, env: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := PrepareTestConfig(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = PrepareTestConfig() })
			t.Setenv("OVN_TRAFFIC_CLASS_ENABLE", tc.env)
			OVNKubernetesFeature.EnableEgressIP = false
			app := cli.NewApp()
			app.Flags = Flags
			app.Action = func(*cli.Context) error {
				return buildOVNKubernetesFeatureConfig(&cliConfig, &config{OVNKubernetesFeature: savedOVNKubernetesFeature})
			}
			err := app.Run(append([]string{"gate-test"}, tc.args...))
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, want error %t", err, tc.wantError)
			}
			if !tc.wantError && (OVNKubernetesFeature.EnableTrafficClass != tc.enabled || OVNKubernetesFeature.EnableEgressIP != tc.egressIP) {
				t.Fatalf("gate=%t EgressIP=%t", OVNKubernetesFeature.EnableTrafficClass, OVNKubernetesFeature.EnableEgressIP)
			}
			if tc.wantError && OVNKubernetesFeature.EnableEgressIP {
				t.Fatal("TrafficClass implicitly enabled EgressIP")
			}
		})
	}
}
