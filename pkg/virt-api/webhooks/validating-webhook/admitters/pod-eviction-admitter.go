package admitters

import (
	"context"
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/types"

	admissionv1 "k8s.io/api/admission/v1"
	k8scorev1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes"

	virtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/kubevirt/pkg/util/migrations"
	validating_webhooks "kubevirt.io/kubevirt/pkg/util/webhooks/validating-webhooks"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

type PodEvictionAdmitter struct {
	clusterConfig *virtconfig.ClusterConfig
	kubeClient    kubernetes.Interface
	virtClient    kubecli.KubevirtClient
}

func NewPodEvictionAdmitter(clusterConfig *virtconfig.ClusterConfig, kubeClient kubernetes.Interface, virtClient kubecli.KubevirtClient) *PodEvictionAdmitter {
	return &PodEvictionAdmitter{
		clusterConfig: clusterConfig,
		kubeClient:    kubeClient,
		virtClient:    virtClient,
	}
}

func (admitter *PodEvictionAdmitter) Admit(ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	pod, err := admitter.kubeClient.CoreV1().Pods(ar.Request.Namespace).Get(context.Background(), ar.Request.Name, metav1.GetOptions{})
	if err != nil {
		return validating_webhooks.NewPassingAdmissionResponse()
	}

	if !isVirtLauncher(pod) {
		return validating_webhooks.NewPassingAdmissionResponse()
	}

	vmiName, exists := pod.GetAnnotations()[virtv1.DomainAnnotation]
	if !exists {
		return validating_webhooks.NewPassingAdmissionResponse()
	}

	vmi, err := admitter.virtClient.VirtualMachineInstance(ar.Request.Namespace).Get(context.Background(), vmiName, metav1.GetOptions{})
	if err != nil {
		return denied(fmt.Sprintf("kubevirt failed getting the vmi: %s", err.Error()))
	}

	evictionStrategy := migrations.VMIEvictionStrategy(admitter.clusterConfig, vmi)
	if evictionStrategy == nil {
		// we don't act on VMIs without an eviction strategy
		return validating_webhooks.NewPassingAdmissionResponse()
	}

	markForEviction := false

	switch *evictionStrategy {
	case virtv1.EvictionStrategyLiveMigrate:
		if !vmi.IsMigratable() {
			return denied(fmt.Sprintf("VMI %s is configured with an eviction strategy but is not live-migratable", vmi.Name))
		}
		markForEviction = true
	case virtv1.EvictionStrategyLiveMigrateIfPossible:
		if vmi.IsMigratable() {
			markForEviction = true
		}
	case virtv1.EvictionStrategyExternal:
		markForEviction = true
	}

	if markForEviction && !vmi.IsMarkedForEviction() && vmi.Status.NodeName == pod.Spec.NodeName {
		dryRun := ar.Request.DryRun != nil && *ar.Request.DryRun == true
		err := admitter.markVMI(vmi.Namespace, vmi.Name, vmi.Status.NodeName, dryRun)
		if err != nil {
			// As with the previous case, it is up to the user to issue a retry.
			return denied(fmt.Sprintf("kubevirt failed marking the vmi for eviction: %s", err.Error()))
		}

		return denied(fmt.Sprintf("Eviction triggered evacuation of VMI \"%s/%s\"", vmi.Namespace, vmi.Name))
	}

	// We can let the request go through because the pod is protected by a PDB if the VMI wants to be live-migrated on
	// eviction. Otherwise, we can just evict it.
	return validating_webhooks.NewPassingAdmissionResponse()
}

func (admitter *PodEvictionAdmitter) markVMI(vmiNamespace, vmiName, nodeName string, dryRun bool) error {
	data := fmt.Sprintf(`[{ "op": "add", "path": "/status/evacuationNodeName", "value": "%s" }]`, nodeName)

	var patchOptions metav1.PatchOptions
	if dryRun {
		patchOptions.DryRun = []string{metav1.DryRunAll}
	}

	_, err := admitter.
		virtClient.
		VirtualMachineInstance(vmiNamespace).
		Patch(context.Background(),
			vmiName,
			types.JSONPatchType,
			[]byte(data),
			patchOptions,
		)

	return err
}

func denied(message string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Message: message,
			Code:    http.StatusTooManyRequests,
		},
	}
}

func isVirtLauncher(pod *k8scorev1.Pod) bool {
	return pod.Labels[virtv1.AppLabel] == "virt-launcher"
}
