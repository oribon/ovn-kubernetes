// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
)

// UnsupportedProtocol identifies an entry decoded by an older client as {}.
type UnsupportedProtocol struct {
	Class    string
	Match    int
	Protocol int
}

// TrafficResolution separates selector resolution from per-pod assignment.
type TrafficResolution struct {
	Destinations []Destination
	ClassCount   int
	Unsupported  []UnsupportedProtocol
}

// ResolveTraffic combines the supported matches of the selected classes.
func ResolveTraffic(eip *eipv1.EgressIP, classes []*trafficv1.TrafficClass) (TrafficResolution, error) {
	result := TrafficResolution{}
	selector, err := ParseTrafficSelector(eip.Spec.TrafficSelector)
	if err != nil {
		return result, err
	}
	var raw []trafficv1.TrafficMatch
	for _, class := range classes {
		if !selector.Matches(labels.Set(class.Labels)) {
			continue
		}
		result.ClassCount++
		raw = append(raw, class.Spec.Matches...)
		for i, match := range class.Spec.Matches {
			for j, protocol := range match.Protocols {
				if protocol.TCP == nil && protocol.UDP == nil && protocol.SCTP == nil {
					result.Unsupported = append(result.Unsupported, UnsupportedProtocol{Class: class.Name, Match: i, Protocol: j})
				}
			}
		}
	}
	result.Destinations, err = ParseDestinations(raw)
	return result, err
}

// TrafficCandidate is one selective owner's resolved traffic for a pod.
type TrafficCandidate struct {
	EgressIP     *eipv1.EgressIP
	Destinations []Destination
}

// TrafficConflict identifies the accepted owner that rejects a candidate.
type TrafficConflict struct {
	Owner       string
	Destination Destination
	Other       Destination
}

// SelectTrafficOwners returns conflicts keyed by candidate owner, with nil for winners.
// Rejected candidates cannot block subsequent candidates.
func SelectTrafficOwners(candidates []TrafficCandidate) map[string]*TrafficConflict {
	candidates = slices.Clone(candidates)
	slices.SortFunc(candidates, func(a, b TrafficCandidate) int {
		if a.EgressIP.CreationTimestamp.Before(&b.EgressIP.CreationTimestamp) {
			return -1
		}
		if b.EgressIP.CreationTimestamp.Before(&a.EgressIP.CreationTimestamp) {
			return 1
		}
		return strings.Compare(a.EgressIP.Name, b.EgressIP.Name)
	})
	selected := make(map[string]*TrafficConflict, len(candidates))
	var winners []TrafficCandidate
	for _, candidate := range candidates {
		conflict := findTrafficConflict(candidate, winners)
		selected[candidate.EgressIP.Name] = conflict
		if conflict == nil {
			winners = append(winners, candidate)
		}
	}
	return selected
}

func findTrafficConflict(candidate TrafficCandidate, winners []TrafficCandidate) *TrafficConflict {
	for _, winner := range winners {
		for _, own := range candidate.Destinations {
			for _, other := range winner.Destinations {
				if own.Overlaps(other) {
					return &TrafficConflict{Owner: winner.EgressIP.Name, Destination: own, Other: other}
				}
			}
		}
	}
	return nil
}

// SelectPodTraffic resolves all selective peers for a pod, independent of local applied state.
func SelectPodTraffic(pod *corev1.Pod, namespaceLabels map[string]string, eips []*eipv1.EgressIP, classes []*trafficv1.TrafficClass) (map[string]*TrafficConflict, error) {
	var candidates []TrafficCandidate
	for _, eip := range eips {
		if !HasTrafficSelector(eip.Spec) {
			continue
		}
		ns, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("EgressIP %s namespace selector: %w", eip.Name, err)
		}
		ps, err := metav1.LabelSelectorAsSelector(&eip.Spec.PodSelector)
		if err != nil {
			return nil, fmt.Errorf("EgressIP %s pod selector: %w", eip.Name, err)
		}
		if !ns.Matches(labels.Set(namespaceLabels)) || !ps.Matches(labels.Set(pod.Labels)) {
			continue
		}
		resolution, err := ResolveTraffic(eip, classes)
		if err != nil {
			if _, selectorErr := ParseTrafficSelector(eip.Spec.TrafficSelector); selectorErr != nil {
				continue
			}
			return nil, fmt.Errorf("EgressIP %s traffic: %w", eip.Name, err)
		}
		if len(resolution.Destinations) != 0 {
			candidates = append(candidates, TrafficCandidate{EgressIP: eip, Destinations: resolution.Destinations})
		}
	}
	return SelectTrafficOwners(candidates), nil
}
