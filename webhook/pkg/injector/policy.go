// Copyright 2026 The gVisor Authors.
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

package injector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gvisor.dev/gvisor/webhook/pkg/gpushare"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclientset "k8s.io/client-go/kubernetes"
)

// nsLabelTTL is how long a namespace's labels are believed.
//
// A label that opts a namespace into a request shape is set by an
// administrator and essentially never changes, so this trades a short window
// of staleness -- during which a newly labelled namespace still has its pods
// refused -- for not calling the API server on a loop.
const nsLabelTTL = 30 * time.Second

// nsLabels reads namespace labels through the API server, remembering each
// answer for nsLabelTTL.
//
// The lookup runs only for a pod that policy would otherwise refuse, which is
// the rare case; a pod asking for an ordinary share of a GPU never causes one.
// The cache is therefore here to keep a rejected Deployment's retry loop from
// hammering the API server, not to make the common path fast.
type nsLabels struct {
	client kubeclientset.Interface

	mu      sync.Mutex
	entries map[string]nsLabelEntry
}

type nsLabelEntry struct {
	labels map[string]string
	read   time.Time
}

func newNSLabels(client kubeclientset.Interface) *nsLabels {
	return &nsLabels{
		client:  client,
		entries: make(map[string]nsLabelEntry),
	}
}

// get returns the labels on a namespace.
//
// An error is returned rather than an empty set, because the caller uses this
// to decide whether to refuse a pod and must not read a failed lookup as an
// absent label: that would turn an unreachable API server into a silent
// opt-in, which is the same failure this webhook's FailurePolicy exists to
// avoid.
func (n *nsLabels) get(ctx context.Context, name string) (map[string]string, error) {
	n.mu.Lock()
	entry, ok := n.entries[name]
	n.mu.Unlock()
	if ok && time.Since(entry.read) < nsLabelTTL {
		return entry.labels, nil
	}

	ns, err := n.client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get namespace %q: %w", name, err)
	}

	n.mu.Lock()
	n.entries[name] = nsLabelEntry{labels: ns.Labels, read: time.Now()}
	n.mu.Unlock()
	return ns.Labels, nil
}

// allowsMultiDeviceFractions reports whether a namespace is labelled to permit
// its pods to hold a fraction of more than one GPU.
func (n *nsLabels) allowsMultiDeviceFractions(ctx context.Context, namespace string) (bool, error) {
	// A pod created without a namespace is being admitted into "default",
	// which is what the API server will fill in; look that up rather than
	// guessing an answer.
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	labels, err := n.get(ctx, namespace)
	if err != nil {
		return false, err
	}
	return labels[gpushare.MultiDeviceFractionLabel] == gpushare.MultiDeviceFractionAllowed, nil
}
