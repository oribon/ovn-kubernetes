// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"github.com/urfave/cli/v2"
)

func TestTrafficClassGateImpliesEgressIP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		env     string
		enabled bool
	}{
		{"default", nil, "", false},
		{"flag", []string{"--enable-egress-traffic-class"}, "", true},
		{"deployment environment", nil, "true", true},
		{"explicitly disabled", []string{"--enable-egress-traffic-class=false"}, "true", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := PrepareTestConfig(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = PrepareTestConfig() })
			t.Setenv("OVN_EGRESS_TRAFFIC_CLASS_ENABLE", tc.env)
			OVNKubernetesFeature.EnableEgressIP = false
			app := cli.NewApp()
			app.Flags = Flags
			app.Action = func(*cli.Context) error {
				return buildOVNKubernetesFeatureConfig(&cliConfig, &config{OVNKubernetesFeature: savedOVNKubernetesFeature})
			}
			if err := app.Run(append([]string{"gate-test"}, tc.args...)); err != nil {
				t.Fatal(err)
			}
			if OVNKubernetesFeature.EnableEgressTrafficClass != tc.enabled || OVNKubernetesFeature.EnableEgressIP != tc.enabled {
				t.Fatalf("gate=%t EgressIP=%t, want %t", OVNKubernetesFeature.EnableEgressTrafficClass, OVNKubernetesFeature.EnableEgressIP, tc.enabled)
			}
		})
	}
}
