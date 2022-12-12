package controllers

import (
	"context"
	"fmt"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	routev1 "github.com/openshift/api/route/v1"
	aiv1beta1 "github.com/openshift/assisted-service/api/v1beta1"
	"github.com/openshift/assisted-service/internal/spoke_k8s_client"
	conditionsv1 "github.com/openshift/custom-resource-status/conditions/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/thoas/go-funk"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var _ = Describe("HypershiftAgentServiceConfig reconcile", func() {
	var (
		ctx                     = context.Background()
		hr                      *HypershiftAgentServiceConfigReconciler
		hsc                     *aiv1beta1.HypershiftAgentServiceConfig
		crd                     *apiextensionsv1.CustomResourceDefinition
		imageServiceStatefulSet *appsv1.StatefulSet
		kubeconfigSecret        *corev1.Secret
		mockCtrl                *gomock.Controller
		mockSpokeClient         *spoke_k8s_client.MockSpokeK8sClient
		mockSpokeClientCache    *MockSpokeClientCache
		fakeSpokeClient         client.WithWatch
	)

	const (
		testKubeconfigSecretName = "test-secret"
		testCRDName              = "agent-install"
	)

	newHypershiftAgentServiceConfigRequest := func(asc *aiv1beta1.HypershiftAgentServiceConfig) ctrl.Request {
		namespacedName := types.NamespacedName{
			Namespace: asc.ObjectMeta.Namespace,
			Name:      asc.ObjectMeta.Name,
		}
		return ctrl.Request{NamespacedName: namespacedName}
	}

	newHSCDefault := func() *aiv1beta1.HypershiftAgentServiceConfig {
		baseAsc := newASCDefault()
		return &aiv1beta1.HypershiftAgentServiceConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testName,
				Namespace: testNamespace,
			},
			Spec: aiv1beta1.HypershiftAgentServiceConfigSpec{
				AgentServiceConfigSpec: aiv1beta1.AgentServiceConfigSpec{
					FileSystemStorage: baseAsc.Spec.FileSystemStorage,
					DatabaseStorage:   baseAsc.Spec.DatabaseStorage,
					ImageStorage:      baseAsc.Spec.ImageStorage,
				},

				KubeconfigSecretRef: corev1.LocalObjectReference{
					Name: testKubeconfigSecretName,
				},
			},
		}
	}

	newKubeconfigSecret := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testKubeconfigSecretName,
				Namespace: testNamespace,
			},
			Data: map[string][]byte{
				"kubeconfig": []byte(BASIC_KUBECONFIG),
			},
			Type: corev1.SecretTypeOpaque,
		}
	}

	newHSCTestReconciler := func(mockSpokeClientCache *MockSpokeClientCache, initObjs ...runtime.Object) *HypershiftAgentServiceConfigReconciler {
		schemes := GetKubeClientSchemes()
		c := fakeclient.NewClientBuilder().WithScheme(schemes).WithRuntimeObjects(initObjs...).Build()
		return &HypershiftAgentServiceConfigReconciler{
			AgentServiceConfigReconcileContext: AgentServiceConfigReconcileContext{
				Client: c,
				Scheme: schemes,
				Log:    logrus.New(),
				// TODO(djzager): If we need to verify emitted events
				// https://github.com/kubernetes/kubernetes/blob/ea0764452222146c47ec826977f49d7001b0ea8c/pkg/controller/statefulset/stateful_pod_control_test.go#L474
				Recorder: record.NewFakeRecorder(10),
			},
			SpokeClients: mockSpokeClientCache,
		}
	}

	newAgentInstallCRD := func() *apiextensionsv1.CustomResourceDefinition {
		c := &apiextensionsv1.CustomResourceDefinition{
			TypeMeta: metav1.TypeMeta{},
			ObjectMeta: metav1.ObjectMeta{
				Name: testCRDName,
				Labels: map[string]string{
					fmt.Sprintf("operators.coreos.com/assisted-service-operator.%s", testNamespace): "",
				},
			},
			Spec:   apiextensionsv1.CustomResourceDefinitionSpec{},
			Status: apiextensionsv1.CustomResourceDefinitionStatus{},
		}
		c.ResourceVersion = ""
		return c
	}

	newImageServiceStatefulSet := func(imageStorage corev1.PersistentVolumeClaimSpec) *appsv1.StatefulSet {
		var replicas int32 = 1
		return &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "assisted-image-service",
				Namespace: testNamespace,
			},
			Spec: appsv1.StatefulSetSpec{
				Replicas: &replicas,
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "image-service-data",
						},
						Spec: imageStorage,
					},
				},
			},
			Status: appsv1.StatefulSetStatus{
				Replicas:        1,
				ReadyReplicas:   1,
				CurrentReplicas: 1,
				UpdatedReplicas: 1,
			},
		}
	}

	ingressCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultIngressCertCMName,
			Namespace: defaultIngressCertCMNamespace,
		},
	}

	route := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: testNamespace,
		},
		Spec: routev1.RouteSpec{
			Host: testHost,
		},
	}

	imageRoute := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      imageServiceName,
			Namespace: testNamespace,
		},
		Spec: routev1.RouteSpec{
			Host: fmt.Sprintf("%s.images", testHost),
		},
	}

	assertReconcileSuccess := func() {
		schemes := GetKubeClientSchemes()
		fakeSpokeClient = fakeclient.NewClientBuilder().WithScheme(schemes).Build()
		client := fakeSpokeK8sClient{Client: fakeSpokeClient}

		mockSpokeClientCache.EXPECT().Get(gomock.Any()).Return(client, nil)
		res, err := hr.Reconcile(ctx, newHypershiftAgentServiceConfigRequest(hsc))
		Expect(err).To(BeNil())
		Expect(res).To(Equal(ctrl.Result{}))
	}

	getHASCInstance := func() *aiv1beta1.HypershiftAgentServiceConfig {
		instance := &aiv1beta1.HypershiftAgentServiceConfig{}
		err := hr.Get(ctx, types.NamespacedName{Name: testName, Namespace: testNamespace}, instance)
		Expect(err).To(BeNil())
		return instance
	}

	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		mockSpokeClient = spoke_k8s_client.NewMockSpokeK8sClient(mockCtrl)
		mockSpokeClientCache = NewMockSpokeClientCache(mockCtrl)

		hsc = newHSCDefault()
		kubeconfigSecret = newKubeconfigSecret()
		crd = newAgentInstallCRD()
		imageServiceStatefulSet = newImageServiceStatefulSet(*hsc.Spec.ImageStorage)
		hr = newHSCTestReconciler(mockSpokeClientCache, hsc, kubeconfigSecret, crd, ingressCM, route, imageRoute, imageServiceStatefulSet)
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	It("runs without error", func() {
		mockSpokeClientCache.EXPECT().Get(gomock.Any()).Return(mockSpokeClient, nil)
		mockSpokeClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		mockSpokeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		crdKey := client.ObjectKeyFromObject(crd)
		Expect(hr.Client.Get(ctx, crdKey, crd)).To(Succeed())
		res, err := hr.Reconcile(ctx, newHypershiftAgentServiceConfigRequest(hsc))
		Expect(err).To(BeNil())
		Expect(res).To(Equal(ctrl.Result{}))
	})

	It("fails due to missing kubeconfig secret", func() {
		Expect(hr.Client.Delete(ctx, kubeconfigSecret)).To(Succeed())
		_, err := hr.Reconcile(ctx, newHypershiftAgentServiceConfigRequest(hsc))
		Expect(err).ToNot(BeNil())
		Expect(err.Error()).To(ContainSubstring(
			fmt.Sprintf("Failed to get '%s' secret in '%s' namespace",
				hsc.Spec.KubeconfigSecretRef.Name, testNamespace)))
	})

	It("fails due to invalid key in kubeconfig secret", func() {
		hsc.Spec.KubeconfigSecretRef.Name = "invalid"
		secret := newKubeconfigSecret()
		secret.ObjectMeta.Name = hsc.Spec.KubeconfigSecretRef.Name
		secret.Data = map[string][]byte{
			"invalid": []byte(BASIC_KUBECONFIG),
		}
		Expect(hr.Client.Create(ctx, secret)).To(Succeed())
		_, err := hr.createSpokeClient(ctx, secret.Name, secret.Namespace)
		Expect(err).ToNot(BeNil())
		Expect(err.Error()).To(ContainSubstring(
			fmt.Sprintf("Secret '%s' does not contain '%s' key value",
				hsc.Spec.KubeconfigSecretRef.Name, "kubeconfig")))
	})

	It("fails due to an error getting client", func() {
		mockSpokeClientCache.EXPECT().Get(gomock.Any()).Return(mockSpokeClient, errors.Errorf("error"))
		_, err := hr.Reconcile(ctx, newHypershiftAgentServiceConfigRequest(hsc))
		Expect(err).ToNot(BeNil())
		Expect(err.Error()).To(ContainSubstring("Failed to create client"))
	})

	It("fails due to missing agent-install CRDs on management cluster", func() {
		mockSpokeClientCache.EXPECT().Get(gomock.Any()).Return(mockSpokeClient, nil)
		Expect(hr.Client.Delete(ctx, crd)).To(Succeed())
		_, err := hr.Reconcile(ctx, newHypershiftAgentServiceConfigRequest(hsc))
		Expect(err).ToNot(BeNil())
		Expect(err.Error()).To(ContainSubstring("agent-install CRDs are not available"))

		instance := getHASCInstance()

		// ReconcileCompleted condition
		condition := conditionsv1.FindStatusCondition(instance.Status.Conditions, aiv1beta1.ConditionReconcileCompleted)
		Expect(condition).ToNot(BeNil())
		Expect(condition.Status).To(Equal(corev1.ConditionFalse))
	})

	It("ignores error listing CRD on spoke cluster (warns for failed cleanup)", func() {
		mockSpokeClientCache.EXPECT().Get(gomock.Any()).Return(mockSpokeClient, nil)
		mockSpokeClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		mockSpokeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(errors.New("error"))
		res, err := hr.Reconcile(ctx, newHypershiftAgentServiceConfigRequest(hsc))
		Expect(err).To(BeNil())
		Expect(res).To(Equal(ctrl.Result{}))
	})

	It("successfully creates CRD on spoke cluster", func() {
		mockSpokeClientCache.EXPECT().Get(gomock.Any()).Return(mockSpokeClient, nil)
		notFoundError := k8serrors.NewNotFound(schema.GroupResource{Group: "v1", Resource: "CustomResourceDefinition"}, testCRDName)
		mockSpokeClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any()).Return(notFoundError).AnyTimes()
		mockSpokeClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		mockSpokeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
		res, err := hr.Reconcile(ctx, newHypershiftAgentServiceConfigRequest(hsc))
		Expect(err).To(BeNil())
		Expect(res).To(Equal(ctrl.Result{}))
	})

	It("successfully updates existing CRD on spoke cluster", func() {
		schemes := GetKubeClientSchemes()
		spokeCRD := newAgentInstallCRD()
		fakeSpokeClient = fakeclient.NewClientBuilder().WithScheme(schemes).WithRuntimeObjects(spokeCRD).Build()

		c := crd.DeepCopy()
		c.Labels["new"] = "label"
		Expect(hr.Client.Update(ctx, c)).To(Succeed())
		mockSpokeClientCache.EXPECT().Get(gomock.Any()).Return(fakeSpokeK8sClient{Client: fakeSpokeClient}, nil)
		res, err := hr.Reconcile(ctx, newHypershiftAgentServiceConfigRequest(hsc))
		Expect(err).To(BeNil())
		Expect(res).To(Equal(ctrl.Result{}))

		crdKey := client.ObjectKeyFromObject(crd)
		spokeCrd := apiextensionsv1.CustomResourceDefinition{}
		Expect(fakeSpokeClient.Get(ctx, crdKey, &spokeCrd)).To(Succeed())
		Expect(spokeCrd.Labels["new"]).To(Equal("label"))
	})

	It("successfully removes redundant CRD from spoke cluster", func() {
		schemes := GetKubeClientSchemes()
		crd = newAgentInstallCRD()
		crd.Name = "redundant"
		fakeSpokeClient = fakeclient.NewClientBuilder().WithScheme(schemes).WithRuntimeObjects(crd).Build()
		crdKey := client.ObjectKeyFromObject(crd)
		spokeCrd := apiextensionsv1.CustomResourceDefinition{}

		mockSpokeClientCache.EXPECT().Get(gomock.Any()).Return(fakeSpokeK8sClient{Client: fakeSpokeClient}, nil)
		res, err := hr.Reconcile(ctx, newHypershiftAgentServiceConfigRequest(hsc))
		Expect(err).To(BeNil())
		Expect(res).To(Equal(ctrl.Result{}))
		Expect(fakeSpokeClient.Get(ctx, crdKey, &spokeCrd)).To(Not(Succeed()))
	})

	It("successfully added kubeconfig resources to service deployment", func() {
		mockSpokeClientCache.EXPECT().Get(gomock.Any()).Return(mockSpokeClient, nil)
		mockSpokeClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		mockSpokeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		crdKey := client.ObjectKeyFromObject(crd)
		hubClient := hr.Client
		Expect(hubClient.Get(ctx, crdKey, crd)).To(Succeed())
		res, err := hr.Reconcile(ctx, newHypershiftAgentServiceConfigRequest(hsc))
		Expect(err).To(BeNil())
		Expect(res).To(Equal(ctrl.Result{}))

		found := &appsv1.Deployment{}
		Expect(hubClient.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: hsc.Namespace}, found)).To(Succeed())
		Expect(found.Spec.Template.Spec.Containers[0].Env).To(ContainElement(
			corev1.EnvVar{
				Name:  "KUBECONFIG",
				Value: "/etc/kube/kubeconfig",
			},
		))
		Expect(found.Spec.Template.Spec.Containers[0].VolumeMounts).To(ContainElement(
			corev1.VolumeMount{
				Name:      "kubeconfig",
				MountPath: "/etc/kube",
			},
		))
		Expect(found.Spec.Template.Spec.Volumes).To(ContainElement(
			corev1.Volume{
				Name: "kubeconfig",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: testKubeconfigSecretName,
					},
				},
			},
		))
	})

	It("successfully creates namespace on spoke cluster", func() {
		assertReconcileSuccess()
		found := &corev1.Namespace{}
		Expect(fakeSpokeClient.Get(ctx, types.NamespacedName{Name: hsc.Namespace}, found)).To(Succeed())
	})

	It("successfully creates service account on spoke cluster", func() {
		assertReconcileSuccess()
		found := &corev1.ServiceAccount{}
		Expect(fakeSpokeClient.Get(ctx, types.NamespacedName{Name: "assisted-service", Namespace: hsc.Namespace}, found)).To(Succeed())
	})

	It("reconcile doesn't change client on AgentServiceConfigReconcileContext", func() {
		client := hr.Client
		assertReconcileSuccess()
		Expect(hr.Client).To(Equal(client))
	})

	It("adds finalizer to HASC", func() {
		assertReconcileSuccess()
		instance := &aiv1beta1.HypershiftAgentServiceConfig{}
		Expect(hr.Client.Get(ctx, types.NamespacedName{Name: hsc.Name, Namespace: hsc.Namespace}, instance)).To(Succeed())
		Expect(funk.ContainsString(instance.GetFinalizers(), agentServiceConfigFinalizerName)).To(BeTrue())
	})

	It("successfully creates image-service statefulSet", func() {
		assertReconcileSuccess()
		found := &appsv1.StatefulSet{}
		Expect(hr.Client.Get(ctx, types.NamespacedName{Name: imageServiceName, Namespace: hsc.Namespace}, found)).To(Succeed())
	})

	It("should set conditions to `True` on successful reconcile", func() {
		assertReconcileSuccess()
		instance := getHASCInstance()

		// ReconcileCompleted condition
		condition := conditionsv1.FindStatusCondition(instance.Status.Conditions, aiv1beta1.ConditionReconcileCompleted)
		Expect(condition).ToNot(BeNil())
		Expect(condition.Status).To(Equal(corev1.ConditionTrue))

		// DeploymentsHealthy condition
		condition = conditionsv1.FindStatusCondition(instance.Status.Conditions, aiv1beta1.ConditionDeploymentsHealthy)
		Expect(condition).ToNot(BeNil())
		Expect(condition.Status).To(Equal(corev1.ConditionTrue))
	})

	Context("parsing rbac", func() {
		var asc ASC

		validateObjectMeta := func(obj client.Object, name, namespace string) {
			Expect(obj.GetName()).To(Equal(name))
			Expect(obj.GetNamespace()).To(Equal(namespace))
		}

		validateRoleUpdate := func(mutateFn controllerutil.MutateFn, cr *rbacv1.Role) {
			cr.Rules = nil
			_ = mutateFn()
			Expect(cr.Rules).NotTo((BeNil()))
		}

		validateClusterRoleUpdate := func(mutateFn controllerutil.MutateFn, cr *rbacv1.ClusterRole) {
			cr.Rules = nil
			_ = mutateFn()
			Expect(cr.Rules).NotTo((BeNil()))
		}

		validateSubjectUpdate := func(mutateFn controllerutil.MutateFn, cr *rbacv1.RoleBinding) {
			cr.Subjects = nil
			cr.RoleRef = rbacv1.RoleRef{}
			_ = mutateFn()
			Expect(cr.Subjects).NotTo((BeNil()))
			Expect(cr.RoleRef.Name).NotTo(BeEmpty())
		}

		validateClusterSubjectUpdate := func(mutateFn controllerutil.MutateFn, cr *rbacv1.ClusterRoleBinding) {
			cr.Subjects = nil
			cr.RoleRef = rbacv1.RoleRef{}
			_ = mutateFn()
			Expect(cr.Subjects).NotTo((BeNil()))
			Expect(cr.RoleRef.Name).NotTo(BeEmpty())
		}

		It("successfully for leader election role", func() {
			asc.initHASC(hr, hsc)
			obj, mutateFn, err := newAssistedServiceRole(ctx, hr.Log, asc)
			Expect(err).To(BeNil())
			validateObjectMeta(obj, "assisted-service", testNamespace)
			Expect(obj.(*rbacv1.Role).Rules).NotTo((BeNil()))
			validateRoleUpdate(mutateFn, obj.(*rbacv1.Role)) //test mutate
		})
		It("successfully for leader election role binding", func() {
			asc.initHASC(hr, hsc)
			obj, mutateFn, err := newAssistedServiceRoleBinding(ctx, hr.Log, asc)
			Expect(err).To(BeNil())
			validateObjectMeta(obj, "assisted-service", testNamespace)
			Expect(obj.(*rbacv1.RoleBinding).RoleRef.Name).To(Equal("assisted-service"))
			validateSubjectUpdate(mutateFn, obj.(*rbacv1.RoleBinding)) //test mutate
		})
		It("successfully for service cluster role", func() {
			asc.initHASC(hr, hsc)
			obj, mutateFn, err := newAssistedServiceClusterRole(ctx, hr.Log, asc)
			Expect(err).To(BeNil())
			validateObjectMeta(obj, "assisted-service-manager-role", "")
			Expect(obj.(*rbacv1.ClusterRole).Rules).NotTo((BeNil()))
			validateClusterRoleUpdate(mutateFn, obj.(*rbacv1.ClusterRole)) //test mutate
		})
		It("successfully for service cluster role binding", func() {
			asc.initHASC(hr, hsc)
			obj, mutateFn, err := newAssistedServiceClusterRoleBinding(ctx, hr.Log, asc)
			Expect(err).To(BeNil())
			validateObjectMeta(obj, "assisted-service-manager-rolebinding", "")
			Expect(obj.(*rbacv1.ClusterRoleBinding).RoleRef.Name).To(Equal("assisted-service-manager-role"))
			validateClusterSubjectUpdate(mutateFn, obj.(*rbacv1.ClusterRoleBinding)) //test mutate
		})
	})
})

type fakeSpokeK8sClient struct {
	client.Client
}

func (c fakeSpokeK8sClient) CreateSubjectAccessReview(subjectAccessReview *authorizationv1.SelfSubjectAccessReview) (*authorizationv1.SelfSubjectAccessReview, error) {
	return nil, nil
}

func (c fakeSpokeK8sClient) IsActionPermitted(verb string, resource string) (bool, error) {
	return true, nil
}

func (c fakeSpokeK8sClient) ListCsrs() (*certificatesv1.CertificateSigningRequestList, error) {
	return nil, nil
}

func (c fakeSpokeK8sClient) ApproveCsr(csr *certificatesv1.CertificateSigningRequest) error {
	return nil
}

func (c fakeSpokeK8sClient) GetNode(name string) (*corev1.Node, error) {
	return nil, nil
}