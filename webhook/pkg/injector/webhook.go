// Copyright 2020 The gVisor Authors.
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

// Package injector handles mutating webhook operations.
package injector

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/mattbaird/jsonpatch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/webhook/pkg/gpushare"
	admv1 "k8s.io/api/admission/v1"
	admregv1 "k8s.io/api/admissionregistration/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclientset "k8s.io/client-go/kubernetes"
	admregclientv1 "k8s.io/client-go/kubernetes/typed/admissionregistration/v1"
	"k8s.io/client-go/util/retry"
)

const (
	// Name is the name of the admission webhook service. The admission
	// webhook must be exposed in the following service; this is mainly for
	// the server certificate.
	Name = "gvisor-injection-admission-webhook"

	// serviceNamespace is the namespace of the admission webhook service.
	serviceNamespace = "e2e"

	fullName = Name + "." + serviceNamespace + ".svc"
)

// CreateConfiguration creates MutatingWebhookConfiguration and registers the
// webhook admission controller with the kube-apiserver. The webhook will only
// take effect on pods in the namespaces selected by `podNsSelector`. If `podNsSelector`
// is empty, the webhook will take effect on all pods.
func CreateConfiguration(clientset kubeclientset.Interface, selector *metav1.LabelSelector) error {
	// Failing closed is a security property here, not just an availability
	// preference. This webhook is what derives a pod's GPU quota from the
	// request the scheduler admitted; a pod that gets past admission without
	// being mutated carries no dev.gvisor.flag.*-gpu-memory-limit and so runs
	// at the node-wide ceiling configured on the runtime, which is the whole
	// device. Under Ignore that is exactly what an unreachable webhook would
	// produce, and silently. Do not relax this to SideEffectClassNone's usual
	// companion, admregv1.Ignore, without replacing the guarantee some other
	// way.
	fail := admregv1.Fail
	// Mutating the pod under admission is not a side effect, since it is the
	// object being admitted rather than anything outside the request; a dry-run
	// admission therefore needs no special handling. Both of these fields are
	// required in v1 and had defaults in v1beta1.
	none := admregv1.SideEffectClassNone

	config := &admregv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: Name,
		},
		Webhooks: []admregv1.MutatingWebhook{
			{
				Name: fullName,
				ClientConfig: admregv1.WebhookClientConfig{
					Service: &admregv1.ServiceReference{
						Name:      Name,
						Namespace: serviceNamespace,
					},
					CABundle: caCert,
				},
				Rules: []admregv1.RuleWithOperations{
					{
						Operations: []admregv1.OperationType{
							admregv1.Create,
						},
						Rule: admregv1.Rule{
							APIGroups:   []string{"*"},
							APIVersions: []string{"*"},
							Resources:   []string{"pods"},
						},
					},
				},
				FailurePolicy:           &fail,
				SideEffects:             &none,
				AdmissionReviewVersions: []string{"v1"},
				NamespaceSelector:       selector,
			},
		},
	}
	log.Infof("Creating MutatingWebhookConfiguration %q", config.Name)
	configs := clientset.AdmissionregistrationV1().MutatingWebhookConfigurations()
	if _, err := configs.Create(context.TODO(), config, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create MutatingWebhookConfiguration %q: %s", config.Name, err)
		}
		// The configuration outlives this process, but the CA does not: the
		// certificates are generated afresh every time the webhook starts, so
		// a configuration left over from a previous run names a CA that no
		// longer signs anything. The apiserver then refuses to call the
		// webhook at all -- and because it fails closed, every pod in a
		// selected namespace is refused with an x509 error until someone
		// deletes the configuration by hand. Adopt it instead.
		log.Infof("MutatingWebhookConfiguration %q already exists; updating it", config.Name)
		if err := updateConfiguration(configs, config); err != nil {
			return err
		}
	}
	return nil
}

// updateConfiguration rewrites an existing MutatingWebhookConfiguration to
// match config, retrying on the conflict another writer can cause.
func updateConfiguration(configs admregclientv1.MutatingWebhookConfigurationInterface, config *admregv1.MutatingWebhookConfiguration) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := configs.Get(context.TODO(), config.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		// Take the whole webhook list rather than only the CABundle, so that a
		// configuration written by an older build is brought up to date in
		// every field at once. ResourceVersion has to be carried over for the
		// update to be accepted.
		existing.Webhooks = config.Webhooks
		_, err = configs.Update(context.TODO(), existing, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to update MutatingWebhookConfiguration %q: %s", config.Name, err)
	}
	return nil
}

// GetTLSConfig retrieves the CA cert that signed the cert used by the webhook.
func GetTLSConfig() *tls.Config {
	sc, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		log.Warningf("Failed to generate X509 key pair: %v", err)
		os.Exit(1)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{sc},
	}
}

// Admit performs admission checks and mutations on Pods.
func Admit(writer http.ResponseWriter, req *http.Request) {
	review := &admv1.AdmissionReview{}
	if err := json.NewDecoder(req.Body).Decode(review); err != nil {
		log.Infof("Failed with error (%v) to decode Admit request: %+v", err, *req)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	log.Debugf("admitPod: %+v", review)
	var err error
	review.Response, err = admitPod(review.Request)
	if err != nil {
		log.Warningf("admitPod failed: %v", err)
		review.Response = &admv1.AdmissionResponse{
			UID: review.Request.UID,
			Result: &metav1.Status{
				Reason:  metav1.StatusReasonInvalid,
				Message: err.Error(),
			},
		}
		sendResponse(writer, review)
		return
	}

	// v1 requires the response to name the request it answers, and rejects the
	// whole review if it does not. v1beta1 let this be empty.
	review.Response.UID = review.Request.UID

	log.Debugf("Processed admission review: %+v", review)
	sendResponse(writer, review)
}

func sendResponse(writer http.ResponseWriter, response any) {
	b, err := json.Marshal(response)
	if err != nil {
		log.Warningf("Failed with error (%v) to marshal response: %+v", err, response)
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}

	writer.WriteHeader(http.StatusOK)
	writer.Write(b)
}

func admitPod(req *admv1.AdmissionRequest) (*admv1.AdmissionResponse, error) {
	// Verify that the request is indeed a Pod.
	resource := metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	if req.Resource != resource {
		return nil, fmt.Errorf("unexpected resource %+v in pod admission", req.Resource)
	}

	// Decode the request into a Pod.
	pod := &v1.Pod{}
	if err := json.Unmarshal(req.Object.Raw, pod); err != nil {
		return nil, fmt.Errorf("failed to decode pod object %s/%s", req.Namespace, req.Name)
	}

	// Copy first to change it.
	podCopy := pod.DeepCopy()
	updatePod(podCopy)
	patch, err := createPatch(req.Object.Raw, podCopy)
	if err != nil {
		return nil, fmt.Errorf("failed to create patch for pod %s/%s (generatedName: %s)", pod.Namespace, pod.Name, pod.GenerateName)
	}

	log.Debugf("Patched pod %s/%s (generateName: %s): %+v", pod.Namespace, pod.Name, pod.GenerateName, podCopy)
	patchType := admv1.PatchTypeJSONPatch
	return &admv1.AdmissionResponse{
		Allowed:   true,
		Patch:     patch,
		PatchType: &patchType,
	}, nil
}

func updatePod(pod *v1.Pod) {
	gvisor := "gvisor"
	pod.Spec.RuntimeClassName = &gvisor

	// Have runsc enforce whatever share of a GPU the pod was scheduled
	// against, and stand down the limiter that runs inside the container.
	gpushare.InjectMemoryLimit(pod)
	gpushare.InjectAMDMemoryLimit(pod)
	gpushare.InjectWeight(pod)
	gpushare.StandDownHAMi(pod)

	// We don't run SELinux test for gvisor.
	// If SELinuxOptions are specified, this is usually for volume test to pass
	// on SELinux. This can be safely ignored.
	if pod.Spec.SecurityContext != nil && pod.Spec.SecurityContext.SELinuxOptions != nil {
		pod.Spec.SecurityContext.SELinuxOptions = nil
	}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.SecurityContext != nil && c.SecurityContext.SELinuxOptions != nil {
			c.SecurityContext.SELinuxOptions = nil
		}
	}
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		if c.SecurityContext != nil && c.SecurityContext.SELinuxOptions != nil {
			c.SecurityContext.SELinuxOptions = nil
		}
	}
}

func createPatch(old []byte, newObj any) ([]byte, error) {
	new, err := json.Marshal(newObj)
	if err != nil {
		return nil, err
	}
	patch, err := jsonpatch.CreatePatch(old, new)
	if err != nil {
		return nil, err
	}
	return json.Marshal(patch)
}
