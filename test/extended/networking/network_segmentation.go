package networking

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	frameworkpod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	admissionapi "k8s.io/pod-security-admission/api"
	utilnet "k8s.io/utils/net"

	exutil "github.com/openshift/origin/test/extended/util"
)

var _ = Describe("[sig-network][OCPFeatureGate:NetworkSegmentation][Feature:UserDefinedPrimaryNetworks]", func() {
	// TODO: so far, only the isolation tests actually require this PSA ... Feels wrong to run everything priviliged.
	// I've tried to have multiple kubeframeworks (from multiple OCs) running (with different project names) but
	// it didn't work.
	oc := exutil.NewCLIWithPodSecurityLevel("network-segmentation-e2e", admissionapi.LevelPrivileged)
	f := oc.KubeFramework()

	InOVNKubernetesContext(func() {
		const (
			nodeHostnameKey              = "kubernetes.io/hostname"
			port                         = 9000
			defaultPort                  = 8080
			userDefinedNetworkIPv4Subnet = "203.203.0.0/16"
			userDefinedNetworkIPv6Subnet = "2014:100:200::0/60"
			nadName                      = "gryffindor"

			udnCrReadyTimeout = 5 * time.Second
		)

		var (
			cs        clientset.Interface
			nadClient nadclient.K8sCniCncfIoV1Interface
		)

		BeforeEach(func() {
			cs = f.ClientSet

			var err error
			nadClient, err = nadclient.NewForConfig(f.ClientConfig())
			Expect(err).NotTo(HaveOccurred())
		})

		DescribeTableSubtree("created using",
			func(createNetworkFn func(c networkAttachmentConfigParams) error) {

				DescribeTable(
					"can perform east/west traffic between nodes",
					func(
						netConfig networkAttachmentConfigParams,
						clientPodConfig podConfiguration,
						serverPodConfig podConfiguration,
					) {
						if netConfig.topology == "layer3" {
							e2eskipper.Skipf("IPv6 routes are not configured on the tech-preview lane since the cluster config is single-stack IPv4")
						}
						var err error

						netConfig.namespace = f.Namespace.Name
						workerNodes, err := getWorkerNodesOrdered(cs)
						Expect(err).NotTo(HaveOccurred())
						Expect(len(workerNodes)).To(BeNumerically(">=", 1))

						clientPodConfig.namespace = f.Namespace.Name
						clientPodConfig.nodeSelector = map[string]string{nodeHostnameKey: workerNodes[0].Name}
						serverPodConfig.namespace = f.Namespace.Name
						serverPodConfig.nodeSelector = map[string]string{nodeHostnameKey: workerNodes[len(workerNodes)-1].Name}

						By("creating the network")
						netConfig.namespace = f.Namespace.Name
						Expect(createNetworkFn(netConfig)).To(Succeed())

						By("creating client/server pods")
						runUDNPod(cs, f.Namespace.Name, serverPodConfig, nil)
						runUDNPod(cs, f.Namespace.Name, clientPodConfig, nil)

						var serverIP string
						for i, cidr := range strings.Split(netConfig.cidr, ",") {
							if cidr != "" {
								By("asserting the server pod has an IP from the configured range")
								serverIP, err = podIPsForUserDefinedPrimaryNetwork(
									cs,
									f.Namespace.Name,
									serverPodConfig.name,
									namespacedName(f.Namespace.Name, netConfig.name),
									i,
								)
								Expect(err).NotTo(HaveOccurred())
								const netPrefixLengthPerNode = 24
								By(fmt.Sprintf("asserting the server pod IP %v is from the configured range %v/%v", serverIP, cidr, netPrefixLengthPerNode))
								subnet, err := getNetCIDRSubnet(cidr)
								Expect(err).NotTo(HaveOccurred())
								Expect(inRange(subnet, serverIP)).To(Succeed())
							}

							By("asserting the *client* pod can contact the server pod exposed endpoint")
							podShouldReach(oc, clientPodConfig.name, formatHostAndPort(net.ParseIP(serverIP), port))
						}
					},
					Entry(
						"for two pods connected over a L2 dualstack primary UDN",
						networkAttachmentConfigParams{
							name:     nadName,
							topology: "layer2",
							cidr:     fmt.Sprintf("%s,%s", userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
							role:     "primary",
						},
						*podConfig(
							"client-pod",
						),
						*podConfig("server-pod", withCommand(func() []string {
							return httpServerContainerCmd(port)
						})),
					),
					Entry(
						"two pods connected over a L3 dualstack primary UDN",
						networkAttachmentConfigParams{
							name:     nadName,
							topology: "layer3",
							cidr:     fmt.Sprintf("%s,%s", userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
							role:     "primary",
						},
						*podConfig(
							"client-pod",
						),
						*podConfig("server-pod", withCommand(func() []string {
							return httpServerContainerCmd(port)
						})),
					),
				)

				DescribeTable(
					"is isolated from the default network",
					func(
						netConfigParams networkAttachmentConfigParams,
						udnPodConfig podConfiguration,
					) {
						By("Creating second namespace for default network pods")
						defaultNetNamespace := f.Namespace.Name + "-default"
						_, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
							ObjectMeta: metav1.ObjectMeta{
								Name: defaultNetNamespace,
							},
						}, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
						defer func() {
							Expect(cs.CoreV1().Namespaces().Delete(context.Background(), defaultNetNamespace, metav1.DeleteOptions{})).To(Succeed())
						}()

						By("creating the network")
						netConfigParams.namespace = f.Namespace.Name
						Expect(createNetworkFn(netConfigParams)).To(Succeed())
						Expect(err).NotTo(HaveOccurred())

						udnPodConfig.namespace = f.Namespace.Name

						udnPod := runUDNPod(cs, f.Namespace.Name, udnPodConfig, func(pod *v1.Pod) {
							pod.Spec.Containers[0].ReadinessProbe = &v1.Probe{
								ProbeHandler: v1.ProbeHandler{
									HTTPGet: &v1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt32(port),
									},
								},
								InitialDelaySeconds: 1,
								PeriodSeconds:       1,
								FailureThreshold:    1,
							}
							pod.Spec.Containers[0].LivenessProbe = &v1.Probe{
								ProbeHandler: v1.ProbeHandler{
									HTTPGet: &v1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt32(port),
									},
								},
								InitialDelaySeconds: 1,
								PeriodSeconds:       1,
								FailureThreshold:    1,
							}
							// add NET_ADMIN to change pod routes
							pod.Spec.Containers[0].SecurityContext = &v1.SecurityContext{
								Capabilities: &v1.Capabilities{
									Add: []v1.Capability{"NET_ADMIN"},
								},
							}
						})

						const podGenerateName = "udn-test-pod-"
						By("creating default network pod")
						defaultPod := frameworkpod.CreateExecPodOrFail(
							context.Background(),
							f.ClientSet,
							defaultNetNamespace,
							podGenerateName,
							func(pod *v1.Pod) {
								pod.Spec.Containers[0].Args = []string{"netexec"}
								setRuntimeDefaultPSA(pod)
							},
						)

						By("creating default network client pod")
						defaultClientPod := frameworkpod.CreateExecPodOrFail(
							context.Background(),
							f.ClientSet,
							defaultNetNamespace,
							podGenerateName,
							func(pod *v1.Pod) {
								setRuntimeDefaultPSA(pod)
							},
						)

						udnIPv4, udnIPv6, err := podIPsForDefaultNetwork(
							cs,
							f.Namespace.Name,
							udnPod.GetName(),
						)
						Expect(err).NotTo(HaveOccurred())

						for _, destIP := range []string{udnIPv4, udnIPv6} {
							if destIP == "" {
								continue
							}
							// positive case for UDN pod is a successful healthcheck, checked later
							By("checking the default network pod can't reach UDN pod on IP " + destIP)
							Consistently(func() bool {
								return connectToServer(podConfiguration{namespace: defaultPod.Namespace, name: defaultPod.Name}, destIP, port) != nil
							}, 5*time.Second).Should(BeTrue())
						}

						defaultIPv4, defaultIPv6, err := podIPsForDefaultNetwork(
							cs,
							defaultPod.Namespace,
							defaultPod.Name,
						)
						Expect(err).NotTo(HaveOccurred())

						for _, destIP := range []string{defaultIPv4, defaultIPv6} {
							if destIP == "" {
								continue
							}
							By("checking the default network client pod can reach default pod on IP " + destIP)
							Eventually(func() bool {
								return connectToServer(podConfiguration{namespace: defaultClientPod.Namespace, name: defaultClientPod.Name}, destIP, defaultPort) == nil
							}).Should(BeTrue())
							By("checking the UDN pod can't reach the default network pod on IP " + destIP)
							Consistently(func() bool {
								return connectToServer(udnPodConfig, destIP, defaultPort) != nil
							}, 5*time.Second).Should(BeTrue())
						}

						// connectivity check is run every second + 1sec initialDelay
						// By this time we have spent at least 8 seconds doing the above checks
						udnPod, err = cs.CoreV1().Pods(udnPod.Namespace).Get(context.Background(), udnPod.Name, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(udnPod.Status.ContainerStatuses[0].RestartCount).To(Equal(int32(0)))

						By("asserting healthcheck works (kubelet can access the UDN pod)")
						// The pod should be ready
						Expect(podutils.IsPodReady(udnPod)).To(BeTrue())

						// TODO
						//By("checking non-kubelet default network host process can't reach the UDN pod")

						By("asserting UDN pod can't reach host via default network interface")
						// tweak pod route to use default network interface as default
						podAnno, err := unmarshalPodAnnotation(udnPod.Annotations, "default")
						Expect(err).NotTo(HaveOccurred())
						for _, podIP := range podAnno.IPs {
							ipCommand := []string{"exec", udnPod.Name, "--", "ip"}
							if podIP.IP.To4() == nil {
								ipCommand = append(ipCommand, "-6")
							}
							// 1. Find current default route and delete it
							defRoute, err := e2ekubectl.RunKubectl(udnPod.Namespace,
								append(ipCommand, "route", "show", "default")...)
							Expect(err).NotTo(HaveOccurred())
							defRoute = strings.TrimSpace(defRoute)
							if defRoute == "" {
								continue
							}
							framework.Logf("Found default route %v, deleting", defRoute)
							cmd := append(ipCommand, "route", "del")
							_, err = e2ekubectl.RunKubectl(udnPod.Namespace,
								append(cmd, strings.Split(defRoute, " ")...)...)
							Expect(err).NotTo(HaveOccurred())

							// 2. Add a new default route to use default network interface
							gatewayIP := NextIP(Network(podIP).IP)
							_, err = e2ekubectl.RunKubectl(udnPod.Namespace,
								append(ipCommand, "route", "add", "default", "via", gatewayIP.String(), "dev", "eth0")...)
							Expect(err).NotTo(HaveOccurred())
						}
						// Now try to reach the host from the UDN pod
						defaultPodHostIP := udnPod.Status.HostIPs
						for _, hostIP := range defaultPodHostIP {
							By("checking the UDN pod can't reach the host on IP " + hostIP.IP)
							ping := "ping"
							if utilnet.IsIPv6String(hostIP.IP) {
								ping = "ping6"
							}
							Consistently(func() bool {
								_, err := e2ekubectl.RunKubectl(udnPod.Namespace, "exec", udnPod.Name, "--",
									ping, "-c", "1", "-W", "1", hostIP.IP,
								)
								return err == nil
							}, 4*time.Second).Should(BeFalse())
						}

						By("asserting UDN pod can't reach default services via default network interface")
						// route setup is already done, get kapi IPs
						kapi, err := cs.CoreV1().Services("default").Get(context.Background(), "kubernetes", metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						for _, kapiIP := range kapi.Spec.ClusterIPs {
							By("checking the UDN pod can't reach kapi service on IP " + kapiIP)
							Consistently(func() bool {
								return connectToServer(udnPodConfig, kapiIP, int(kapi.Spec.Ports[0].Port)) != nil
							}, 5*time.Second).Should(BeTrue())
						}
					},
					Entry(
						"with L2 dualstack primary UDN",
						networkAttachmentConfigParams{
							name:     nadName,
							topology: "layer2",
							cidr:     fmt.Sprintf("%s,%s", userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
							role:     "primary",
						},
						*podConfig("udn-pod", withCommand(func() []string {
							return httpServerContainerCmd(port)
						})),
					),
					Entry(
						"with L3 dualstack primary UDN",
						networkAttachmentConfigParams{
							name:     nadName,
							topology: "layer3",
							cidr:     fmt.Sprintf("%s,%s", userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
							role:     "primary",
						},
						*podConfig("udn-pod", withCommand(func() []string {
							return httpServerContainerCmd(port)
						})),
					),
				)
			},
			Entry("NetworkAttachmentDefinitions", func(c networkAttachmentConfigParams) error {
				netConfig := newNetworkAttachmentConfig(c)
				nad := generateNAD(netConfig)
				_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(context.Background(), nad, metav1.CreateOptions{})
				return err
			}),
			Entry("UserDefinedNetwork", func(c networkAttachmentConfigParams) error {
				udnManifest := generateUserDefinedNetworkManifest(&c)
				cleanup, err := createManifest(f.Namespace.Name, udnManifest)
				DeferCleanup(cleanup)
				Expect(waitForUserDefinedNetworkReady(f.Namespace.Name, c.name, udnCrReadyTimeout)).To(Succeed())
				return err
			}),
		)
	})
})

func generateUserDefinedNetworkManifest(params *networkAttachmentConfigParams) string {
	nadToUdnParams := map[string]string{
		"primary":   "Primary",
		"secondary": "Secondary",
		"layer2":    "Layer2",
		"layer3":    "Layer3",
	}
	subnets := generateSubnetsYaml(params)
	return `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: ` + params.name + `
spec:
  topology: ` + nadToUdnParams[params.topology] + `
  ` + params.topology + `: 
    role: ` + nadToUdnParams[params.role] + `
    subnets: ` + subnets + `
`
}

func generateSubnetsYaml(params *networkAttachmentConfigParams) string {
	if params.topology == "layer3" {
		l3Subnets := generateLayer3Subnets(params.cidr)
		return fmt.Sprintf("[%s]", strings.Join(l3Subnets, ","))
	}
	return fmt.Sprintf("[%s]", params.cidr)
}

func generateLayer3Subnets(cidrs string) []string {
	cidrList := strings.Split(cidrs, ",")
	var subnets []string
	for _, cidr := range cidrList {
		cidrSplit := strings.Split(cidr, "/")
		switch len(cidrSplit) {
		case 2:
			subnets = append(subnets, fmt.Sprintf(`{cidr: "%s/%s"}`, cidrSplit[0], cidrSplit[1]))
		case 3:
			subnets = append(subnets, fmt.Sprintf(`{cidr: "%s/%s", hostSubnet: %q }`, cidrSplit[0], cidrSplit[1], cidrSplit[2]))
		default:
			panic(fmt.Sprintf("invalid layer3 subnet: %v", cidr))
		}
	}
	return subnets
}

func createManifest(namespace, manifest string) (func(), error) {
	path := "test-ovn-k-udn-" + rand.String(5) + ".yaml"
	if err := os.WriteFile(path, []byte(manifest), 0644); err != nil {
		framework.Failf("Unable to write udn yaml to disk: %v", err)
	}
	cleanup := func() {
		if err := os.Remove(path); err != nil {
			framework.Logf("Unable to remove udn yaml from disk: %v", err)
		}
	}
	_, err := e2ekubectl.RunKubectl(namespace, "create", "-f", path)
	if err != nil {
		return cleanup, err
	}
	return cleanup, nil
}

func waitForUserDefinedNetworkReady(namespace, name string, timeout time.Duration) error {
	_, err := e2ekubectl.RunKubectl(namespace, "wait", "userdefinednetwork", name, "--for", "condition=NetworkReady=True", "--timeout", timeout.String())
	return err
}

func setRuntimeDefaultPSA(pod *v1.Pod) {
	dontEscape := false
	noRoot := true
	pod.Spec.SecurityContext = &v1.PodSecurityContext{
		RunAsNonRoot: &noRoot,
		SeccompProfile: &v1.SeccompProfile{
			Type: v1.SeccompProfileTypeRuntimeDefault,
		},
	}
	pod.Spec.Containers[0].SecurityContext = &v1.SecurityContext{
		AllowPrivilegeEscalation: &dontEscape,
		Capabilities: &v1.Capabilities{
			Drop: []v1.Capability{"ALL"},
		},
	}
}

type podOption func(*podConfiguration)

func podConfig(podName string, opts ...podOption) *podConfiguration {
	pod := &podConfiguration{
		name: podName,
	}
	for _, opt := range opts {
		opt(pod)
	}
	return pod
}

func withCommand(cmdGenerationFn func() []string) podOption {
	return func(pod *podConfiguration) {
		pod.containerCmd = cmdGenerationFn()
	}
}

// podIPsForUserDefinedPrimaryNetwork returns the v4 or v6 IPs for a pod on the UDN
func podIPsForUserDefinedPrimaryNetwork(k8sClient clientset.Interface, podNamespace string, podName string, attachmentName string, index int) (string, error) {
	pod, err := k8sClient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	netStatus, err := userDefinedNetworkStatus(pod, attachmentName)
	if err != nil {
		return "", err
	}

	if len(netStatus.IPs) == 0 {
		return "", fmt.Errorf("attachment for network %q without IPs", attachmentName)
	}
	if len(netStatus.IPs) > 2 {
		return "", fmt.Errorf("attachment for network %q with more than two IPs", attachmentName)
	}
	return netStatus.IPs[index].IP.String(), nil
}

func podIPsForDefaultNetwork(k8sClient clientset.Interface, podNamespace string, podName string) (string, string, error) {
	pod, err := k8sClient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	ipv4, ipv6 := getPodAddresses(pod)
	return ipv4, ipv6, nil
}

func userDefinedNetworkStatus(pod *v1.Pod, networkName string) (PodAnnotation, error) {
	netStatus, err := unmarshalPodAnnotation(pod.Annotations, networkName)
	if err != nil {
		return PodAnnotation{}, fmt.Errorf("failed to unmarshall annotations for pod %q: %v", pod.Name, err)
	}

	return *netStatus, nil
}

func runUDNPod(cs clientset.Interface, namespace string, podConfig podConfiguration, podSpecTweak func(*v1.Pod)) *v1.Pod {
	By(fmt.Sprintf("instantiating the UDN pod %s", podConfig.name))
	podSpec := generatePodSpec(podConfig)
	if podSpecTweak != nil {
		podSpecTweak(podSpec)
	}
	serverPod, err := cs.CoreV1().Pods(podConfig.namespace).Create(
		context.Background(),
		podSpec,
		metav1.CreateOptions{},
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(serverPod).NotTo(BeNil())

	By(fmt.Sprintf("asserting the UDN pod %s reaches the `Ready` state", podConfig.name))
	var updatedPod *v1.Pod
	Eventually(func() v1.PodPhase {
		updatedPod, err = cs.CoreV1().Pods(namespace).Get(context.Background(), serverPod.GetName(), metav1.GetOptions{})
		if err != nil {
			return v1.PodFailed
		}
		return updatedPod.Status.Phase
	}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))
	return updatedPod
}

type networkAttachmentConfigParams struct {
	cidr               string
	excludeCIDRs       []string
	namespace          string
	name               string
	topology           string
	networkName        string
	vlanID             int
	allowPersistentIPs bool
	role               string
}

type networkAttachmentConfig struct {
	networkAttachmentConfigParams
}

func newNetworkAttachmentConfig(params networkAttachmentConfigParams) networkAttachmentConfig {
	networkAttachmentConfig := networkAttachmentConfig{
		networkAttachmentConfigParams: params,
	}
	if networkAttachmentConfig.networkName == "" {
		networkAttachmentConfig.networkName = uniqueNadName(networkAttachmentConfig.name)
	}
	return networkAttachmentConfig
}

func uniqueNadName(originalNetName string) string {
	const randomStringLength = 5
	return fmt.Sprintf("%s_%s", rand.String(randomStringLength), originalNetName)
}

func generateNAD(config networkAttachmentConfig) *nadapi.NetworkAttachmentDefinition {
	nadSpec := fmt.Sprintf(
		`
{
        "cniVersion": "0.3.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology":%q,
        "subnets": %q,
        "excludeSubnets": %q,
        "mtu": 1300,
        "netAttachDefName": %q,
        "vlanID": %d,
        "allowPersistentIPs": %t,
        "role": %q
}
`,
		config.networkName,
		config.topology,
		config.cidr,
		strings.Join(config.excludeCIDRs, ","),
		namespacedName(config.namespace, config.name),
		config.vlanID,
		config.allowPersistentIPs,
		config.role,
	)
	return generateNetAttachDef(config.namespace, config.name, nadSpec)
}

func generateNetAttachDef(namespace, nadName, nadSpec string) *nadapi.NetworkAttachmentDefinition {
	return &nadapi.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nadName,
			Namespace: namespace,
		},
		Spec: nadapi.NetworkAttachmentDefinitionSpec{Config: nadSpec},
	}
}

func namespacedName(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

type podConfiguration struct {
	attachments            []nadapi.NetworkSelectionElement
	containerCmd           []string
	name                   string
	namespace              string
	nodeSelector           map[string]string
	isPrivileged           bool
	labels                 map[string]string
	requiresExtraNamespace bool
}

func generatePodSpec(config podConfiguration) *v1.Pod {
	podSpec := frameworkpod.NewAgnhostPod(config.namespace, config.name, nil, nil, nil, config.containerCmd...)
	if len(config.attachments) > 0 {
		podSpec.Annotations = networkSelectionElements(config.attachments...)
	}
	podSpec.Spec.NodeSelector = config.nodeSelector
	podSpec.Labels = config.labels
	if config.isPrivileged {
		privileged := true
		podSpec.Spec.Containers[0].SecurityContext.Privileged = &privileged
	}
	return podSpec
}

func networkSelectionElements(elements ...nadapi.NetworkSelectionElement) map[string]string {
	marshalledElements, err := json.Marshal(elements)
	if err != nil {
		panic(fmt.Errorf("programmer error: you've provided wrong input to the test data: %v", err))
	}
	return map[string]string{
		nadapi.NetworkAttachmentAnnot: string(marshalledElements),
	}
}

func httpServerContainerCmd(port uint16) []string {
	return []string{"netexec", "--http-port", fmt.Sprintf("%d", port)}
}

func getNetCIDRSubnet(netCIDR string) (string, error) {
	subStrings := strings.Split(netCIDR, "/")
	if len(subStrings) == 3 {
		return subStrings[0] + "/" + subStrings[1], nil
	} else if len(subStrings) == 2 {
		return netCIDR, nil
	}
	return "", fmt.Errorf("invalid network cidr: %q", netCIDR)
}

func inRange(cidr string, ip string) error {
	_, cidrRange, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}

	if cidrRange.Contains(net.ParseIP(ip)) {
		return nil
	}

	return fmt.Errorf("ip [%s] is NOT in range %s", ip, cidr)
}

func connectToServer(clientPodConfig podConfiguration, serverIP string, port int) error {
	_, err := e2ekubectl.RunKubectl(
		clientPodConfig.namespace,
		"exec",
		clientPodConfig.name,
		"--",
		"curl",
		"--connect-timeout",
		"2",
		net.JoinHostPort(serverIP, fmt.Sprintf("%d", port)),
	)
	return err
}

// Returns pod's ipv4 and ipv6 addresses IN ORDER
func getPodAddresses(pod *v1.Pod) (string, string) {
	var ipv4Res, ipv6Res string
	for _, a := range pod.Status.PodIPs {
		if utilnet.IsIPv4String(a.IP) {
			ipv4Res = a.IP
		}
		if utilnet.IsIPv6String(a.IP) {
			ipv6Res = a.IP
		}
	}
	return ipv4Res, ipv6Res
}

// PodAnnotation describes the assigned network details for a single pod network. (The
// actual annotation may include the equivalent of multiple PodAnnotations.)
type PodAnnotation struct {
	// IPs are the pod's assigned IP addresses/prefixes
	IPs []*net.IPNet
	// MAC is the pod's assigned MAC address
	MAC net.HardwareAddr
	// Gateways are the pod's gateway IP addresses; note that there may be
	// fewer Gateways than IPs.
	Gateways []net.IP
	// Routes are additional routes to add to the pod's network namespace
	Routes []PodRoute
	// Primary reveals if this network is the primary network of the pod or not
	Primary bool
}

// PodRoute describes any routes to be added to the pod's network namespace
type PodRoute struct {
	// Dest is the route destination
	Dest *net.IPNet
	// NextHop is the IP address of the next hop for traffic destined for Dest
	NextHop net.IP
}

type annotationNotSetError struct {
	msg string
}

func (anse annotationNotSetError) Error() string {
	return anse.msg
}

// newAnnotationNotSetError returns an error for an annotation that is not set
func newAnnotationNotSetError(format string, args ...interface{}) error {
	return annotationNotSetError{msg: fmt.Sprintf(format, args...)}
}

type podAnnotation struct {
	IPs      []string   `json:"ip_addresses"`
	MAC      string     `json:"mac_address"`
	Gateways []string   `json:"gateway_ips,omitempty"`
	Routes   []podRoute `json:"routes,omitempty"`

	IP      string `json:"ip_address,omitempty"`
	Gateway string `json:"gateway_ip,omitempty"`
	Primary bool   `json:"primary"`
}

type podRoute struct {
	Dest    string `json:"dest"`
	NextHop string `json:"nextHop"`
}

// UnmarshalPodAnnotation returns the default network info from pod.Annotations
func unmarshalPodAnnotation(annotations map[string]string, networkName string) (*PodAnnotation, error) {
	const podNetworkAnnotation = "k8s.ovn.org/pod-networks"
	ovnAnnotation, ok := annotations[podNetworkAnnotation]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod annotation in %v", annotations)
	}

	podNetworks := make(map[string]podAnnotation)
	if err := json.Unmarshal([]byte(ovnAnnotation), &podNetworks); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
			ovnAnnotation, err)
	}
	tempA := podNetworks[networkName]
	a := &tempA

	podAnnotation := &PodAnnotation{Primary: a.Primary}
	var err error

	podAnnotation.MAC, err = net.ParseMAC(a.MAC)
	if err != nil {
		return nil, fmt.Errorf("failed to parse pod MAC %q: %v", a.MAC, err)
	}

	if len(a.IPs) == 0 {
		if a.IP == "" {
			return nil, fmt.Errorf("bad annotation data (neither ip_address nor ip_addresses is set)")
		}
		a.IPs = append(a.IPs, a.IP)
	} else if a.IP != "" && a.IP != a.IPs[0] {
		return nil, fmt.Errorf("bad annotation data (ip_address and ip_addresses conflict)")
	}
	for _, ipstr := range a.IPs {
		ip, ipnet, err := net.ParseCIDR(ipstr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pod IP %q: %v", ipstr, err)
		}
		ipnet.IP = ip
		podAnnotation.IPs = append(podAnnotation.IPs, ipnet)
	}

	if len(a.Gateways) == 0 {
		if a.Gateway != "" {
			a.Gateways = append(a.Gateways, a.Gateway)
		}
	} else if a.Gateway != "" && a.Gateway != a.Gateways[0] {
		return nil, fmt.Errorf("bad annotation data (gateway_ip and gateway_ips conflict)")
	}
	for _, gwstr := range a.Gateways {
		gw := net.ParseIP(gwstr)
		if gw == nil {
			return nil, fmt.Errorf("failed to parse pod gateway %q", gwstr)
		}
		podAnnotation.Gateways = append(podAnnotation.Gateways, gw)
	}

	for _, r := range a.Routes {
		route := PodRoute{}
		_, route.Dest, err = net.ParseCIDR(r.Dest)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pod route dest %q: %v", r.Dest, err)
		}
		if route.Dest.IP.IsUnspecified() {
			return nil, fmt.Errorf("bad podNetwork data: default route %v should be specified as gateway", route)
		}
		if r.NextHop != "" {
			route.NextHop = net.ParseIP(r.NextHop)
			if route.NextHop == nil {
				return nil, fmt.Errorf("failed to parse pod route next hop %q", r.NextHop)
			} else if utilnet.IsIPv6(route.NextHop) != utilnet.IsIPv6CIDR(route.Dest) {
				return nil, fmt.Errorf("pod route %s has next hop %s of different family", r.Dest, r.NextHop)
			}
		}
		podAnnotation.Routes = append(podAnnotation.Routes, route)
	}

	return podAnnotation, nil
}

// Copyright 2015 CNI authors
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

// NextIP returns IP incremented by 1, if IP is invalid, return nil
// copied from https://github.com/containernetworking/plugins/blob/acf8ddc8e1128e6f68a34f7fe91122afeb1fa93d/pkg/ip/cidr.go#L23
func NextIP(ip net.IP) net.IP {
	normalizedIP := normalizeIP(ip)
	if normalizedIP == nil {
		return nil
	}

	i := ipToInt(normalizedIP)
	return intToIP(i.Add(i, big.NewInt(1)), len(normalizedIP) == net.IPv6len)
}

// copied from https://github.com/containernetworking/plugins/blob/acf8ddc8e1128e6f68a34f7fe91122afeb1fa93d/pkg/ip/cidr.go#L60
func ipToInt(ip net.IP) *big.Int {
	return big.NewInt(0).SetBytes(ip)
}

// copied from https://github.com/containernetworking/plugins/blob/acf8ddc8e1128e6f68a34f7fe91122afeb1fa93d/pkg/ip/cidr.go#L64
func intToIP(i *big.Int, isIPv6 bool) net.IP {
	intBytes := i.Bytes()

	if len(intBytes) == net.IPv4len || len(intBytes) == net.IPv6len {
		return intBytes
	}

	if isIPv6 {
		return append(make([]byte, net.IPv6len-len(intBytes)), intBytes...)
	}

	return append(make([]byte, net.IPv4len-len(intBytes)), intBytes...)
}

// normalizeIP will normalize IP by family,
// IPv4 : 4-byte form
// IPv6 : 16-byte form
// others : nil
// copied from https://github.com/containernetworking/plugins/blob/acf8ddc8e1128e6f68a34f7fe91122afeb1fa93d/pkg/ip/cidr.go#L82
func normalizeIP(ip net.IP) net.IP {
	if ipTo4 := ip.To4(); ipTo4 != nil {
		return ipTo4
	}
	return ip.To16()
}

// Network masks off the host portion of the IP, if IPNet is invalid,
// return nil
// copied from https://github.com/containernetworking/plugins/blob/acf8ddc8e1128e6f68a34f7fe91122afeb1fa93d/pkg/ip/cidr.go#L89C1-L105C2
func Network(ipn *net.IPNet) *net.IPNet {
	if ipn == nil {
		return nil
	}

	maskedIP := ipn.IP.Mask(ipn.Mask)
	if maskedIP == nil {
		return nil
	}

	return &net.IPNet{
		IP:   maskedIP,
		Mask: ipn.Mask,
	}
}
