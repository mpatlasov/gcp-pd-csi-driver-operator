package operator

import (
	"context"
	"fmt"
	"time"

	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	opv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	opclient "github.com/openshift/client-go/operator/clientset/versioned"
	opinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/csi/csicontrollerset"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivercontrollerservicecontroller"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivernodeservicecontroller"
	goc "github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/gcp-pd-csi-driver-operator/assets"
)

const (
	// Operand and operator run in the same namespace
	defaultNamespace   = "openshift-cluster-csi-drivers"
	operatorName       = "gcp-pd-csi-driver-operator"
	operandName        = "gcp-pd-csi-driver"
	trustedCAConfigMap = "gcp-pd-csi-driver-trusted-ca-bundle"
	resync             = 20 * time.Minute

	cloudCredSecretName   = "gcp-pd-cloud-credentials"
	metricsCertSecretName = "gcp-pd-csi-driver-controller-metrics-serving-cert"

	diskEncryptionKMSKey  = "disk-encryption-kms-key"
	defaultKMSKeyLocation = "global"
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	// Create core clientset and informers
	kubeClient := kubeclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient, defaultNamespace, "")
	secretInformer := kubeInformersForNamespaces.InformersFor(defaultNamespace).Core().V1().Secrets()
	configMapInformer := kubeInformersForNamespaces.InformersFor(defaultNamespace).Core().V1().ConfigMaps()
	nodeInformer := kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes()

	// Create config clientset and informer. This is used to get the cluster ID
	configClient := configclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	configInformers := configinformers.NewSharedInformerFactory(configClient, resync)
	infraInformer := configInformers.Config().V1().Infrastructures()

	apiExtClient, err := apiextclient.NewForConfig(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	if err != nil {
		return err
	}

	// Create GenericOperatorclient. This is used by the library-go controllers created down below
	gvr := opv1.SchemeGroupVersion.WithResource("clustercsidrivers")
	operatorClient, dynamicInformers, err := goc.NewClusterScopedOperatorClientWithConfigName(controllerConfig.KubeConfig, gvr, string(opv1.GCPPDCSIDriver))
	if err != nil {
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(controllerConfig.KubeConfig)
	if err != nil {
		return err
	}

	// operator.openshift.io client, used for ClusterCSIDriver
	operatorClientSet, err := opclient.NewForConfig(controllerConfig.KubeConfig)
	if err != nil {
		return err
	}
	operatorInformers := opinformers.NewSharedInformerFactory(operatorClientSet, resync)

	csiControllerSet := csicontrollerset.NewCSIControllerSet(
		operatorClient,
		controllerConfig.EventRecorder,
	).WithLogLevelController().WithManagementStateController(
		operandName,
		false,
	).WithStaticResourcesController(
		"GCPPDDriverStaticResourcesController",
		kubeClient,
		dynamicClient,
		kubeInformersForNamespaces,
		assets.ReadFile,
		[]string{
			"csidriver.yaml",
			"controller_sa.yaml",
			"controller_pdb.yaml",
			"node_sa.yaml",
			"service.yaml",
			"cabundle_cm.yaml",
			"rbac/main_attacher_binding.yaml",
			"rbac/privileged_role.yaml",
			"rbac/controller_privileged_binding.yaml",
			"rbac/node_privileged_binding.yaml",
			"rbac/main_provisioner_binding.yaml",
			"rbac/volumesnapshot_reader_provisioner_binding.yaml",
			"rbac/main_resizer_binding.yaml",
			"rbac/storageclass_reader_resizer_binding.yaml",
			"rbac/main_snapshotter_binding.yaml",
			"rbac/kube_rbac_proxy_role.yaml",
			"rbac/kube_rbac_proxy_binding.yaml",
			"rbac/prometheus_role.yaml",
			"rbac/prometheus_rolebinding.yaml",
			"rbac/lease_leader_election_role.yaml",
			"rbac/lease_leader_election_rolebinding.yaml",
		},
	).WithConditionalStaticResourcesController(
		"GCPPDDriverConditionalStaticResourcesController",
		kubeClient,
		dynamicClient,
		kubeInformersForNamespaces,
		assets.ReadFile,
		[]string{
			"volumesnapshotclass.yaml",
		},
		// Only install when CRD exists.
		func() bool {
			name := "volumesnapshotclasses.snapshot.storage.k8s.io"
			_, err := apiExtClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), name, metav1.GetOptions{})
			return err == nil
		},
		// Don't ever remove.
		func() bool {
			return false
		},
	).WithCSIConfigObserverController(
		"GCPPDDriverCSIConfigObserverController",
		configInformers,
	).WithCSIDriverControllerService(
		"GCPPDDriverControllerServiceController",
		assets.ReadFile,
		"controller.yaml",
		kubeClient,
		kubeInformersForNamespaces.InformersFor(defaultNamespace),
		configInformers,
		[]factory.Informer{
			nodeInformer.Informer(),
			infraInformer.Informer(),
			secretInformer.Informer(),
			configMapInformer.Informer(),
		},
		csidrivercontrollerservicecontroller.WithObservedProxyDeploymentHook(),
		csidrivercontrollerservicecontroller.WithCABundleDeploymentHook(
			defaultNamespace,
			trustedCAConfigMap,
			configMapInformer,
		),
		csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(
			defaultNamespace,
			cloudCredSecretName,
			secretInformer,
		),
		csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(
			defaultNamespace,
			metricsCertSecretName,
			secretInformer,
		),
		csidrivercontrollerservicecontroller.WithReplicasHook(nodeInformer.Lister()),
	).WithCSIDriverNodeService(
		"GCPPDDriverNodeServiceController",
		assets.ReadFile,
		"node.yaml",
		kubeClient,
		kubeInformersForNamespaces.InformersFor(defaultNamespace),
		[]factory.Informer{configMapInformer.Informer()},
		csidrivernodeservicecontroller.WithObservedProxyDaemonSetHook(),
		csidrivernodeservicecontroller.WithCABundleDaemonSetHook(
			defaultNamespace,
			trustedCAConfigMap,
			configMapInformer,
		),
	).WithServiceMonitorController(
		"GCPPDDriverServiceMonitorController",
		dynamicClient,
		assets.ReadFile,
		"servicemonitor.yaml",
	).WithStorageClassController(
		"GCPPDDriverStorageClassController",
		assets.ReadFile,
		[]string{
			"storageclass.yaml",
			"storageclass_ssd.yaml",
		},
		kubeClient,
		kubeInformersForNamespaces.InformersFor(""),
		operatorInformers,
		getKMSKeyHook(operatorInformers.Operator().V1().ClusterCSIDrivers().Lister()),
	)

	if err != nil {
		return err
	}

	klog.Info("Starting the informers")
	go kubeInformersForNamespaces.Start(ctx.Done())
	go dynamicInformers.Start(ctx.Done())
	go configInformers.Start(ctx.Done())
	go operatorInformers.Start(ctx.Done())

	klog.Info("Starting controllerset")
	go csiControllerSet.Run(ctx, 1)

	<-ctx.Done()

	return fmt.Errorf("stopped")
}
