package aws

import (
	"context"
	"fmt"
	"io"
	"log"

	dto "github.com/prometheus/client_model/go"
	"github.com/virtual-kubelet/aws-fargate/fargate"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

// FargateProvider implements the virtual-kubelet provider interface.
type FargateProvider struct {
	nodeName           string
	operatingSystem    string
	internalIP         string
	daemonEndpointPort int32

	// AWS resources.
	region         string
	subnets        []string
	securityGroups []string

	// Fargate resources.
	cluster                 *fargate.Cluster
	clusterName             string
	capacity                capacity
	assignPublicIPv4Address bool
	executionRoleArn        string
	cloudWatchLogGroupName  string
	platformVersion         string
}

// Capacity represents the provisioned capacity on a Fargate cluster.
type capacity struct {
	cpu     string
	memory  string
	storage string
	pods    string
}

// NewFargateProvider creates a new Fargate provider.
func NewFargateProvider(
	ctx context.Context,
	config string,
	nodeName string,
	operatingSystem string,
	internalIP string,
	daemonEndpointPort int32) (*FargateProvider, error) {

	// Create the Fargate provider.
	log.Println("Creating Fargate provider.")

	p := FargateProvider{
		nodeName:           nodeName,
		operatingSystem:    operatingSystem,
		internalIP:         internalIP,
		daemonEndpointPort: daemonEndpointPort,
	}

	// Read the Fargate provider configuration file.
	err := p.loadConfigFile(config)
	if err != nil {
		err = fmt.Errorf("failed to load configuration file %s: %v", config, err)
		return nil, err
	}

	log.Printf("Loaded provider configuration file %s.", config)

	// Find or create the configured Fargate cluster.
	clusterConfig := fargate.ClusterConfig{
		Region:                  p.region,
		Name:                    p.clusterName,
		NodeName:                nodeName,
		Subnets:                 p.subnets,
		SecurityGroups:          p.securityGroups,
		AssignPublicIPv4Address: p.assignPublicIPv4Address,
		ExecutionRoleArn:        p.executionRoleArn,
		CloudWatchLogGroupName:  p.cloudWatchLogGroupName,
		PlatformVersion:         p.platformVersion,
	}

	p.cluster, err = fargate.NewCluster(ctx, &clusterConfig)
	if err != nil {
		err = fmt.Errorf("failed to create Fargate cluster: %v", err)
		return nil, err
	}

	log.Printf("Created Fargate provider: %+v.", p)

	return &p, nil
}

// CreatePod takes a Kubernetes Pod and deploys it within the Fargate provider.
func (p *FargateProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	log.Printf("Received CreatePod request for %+v.\n", pod)

	fgPod, err := fargate.NewPod(ctx, p.cluster, pod)
	if err != nil {
		log.Printf("Failed to create pod: %v.\n", err)
		return err
	}

	err = fgPod.Start(ctx)
	if err != nil {
		log.Printf("Failed to start pod: %v.\n", err)
		return err
	}

	return nil
}

// UpdatePod takes a Kubernetes Pod and updates it within the provider.
func (p *FargateProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	log.Printf("Received UpdatePod request for %s/%s.\n", pod.Namespace, pod.Name)
	return errdefs.InvalidInput("updating a pod is not supported by the AWS Fargate provider")
}

// DeletePod takes a Kubernetes Pod and deletes it from the provider.
func (p *FargateProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	log.Printf("Received DeletePod request for %s/%s.\n", pod.Namespace, pod.Name)

	fgPod, err := p.cluster.GetPod(pod.Namespace, pod.Name)
	if err != nil {
		log.Printf("Failed to get pod: %v.\n", err)
		return err
	}

	err = fgPod.Stop(ctx)
	if err != nil {
		log.Printf("Failed to stop pod: %v.\n", err)
		return err
	}

	return nil
}

// GetPod retrieves a pod by name from the provider (can be cached).
func (p *FargateProvider) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	log.Printf("Received GetPod request for %s/%s.\n", namespace, name)

	pod, err := p.cluster.GetPod(namespace, name)
	if err != nil {
		log.Printf("Failed to get pod: %v.\n", err)
		return nil, err
	}

	spec, err := pod.GetSpec(ctx)
	if err != nil {
		log.Printf("Failed to get pod spec: %v.\n", err)
		return nil, err
	}

	return spec, nil
}

// GetContainerLogs retrieves the logs of a container by name from the provider.
func (p *FargateProvider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	log.Printf("Received GetContainerLogs request for %s/%s/%s.\n", namespace, podName, containerName)
	return p.cluster.GetContainerLogs(ctx, namespace, podName, containerName, opts)
}

// RunInContainer executes a command in a container in the pod, copying data
// between in/out/err and the container's stdin/stdout/stderr.
func (p *FargateProvider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error {
	return errdefs.InvalidInput("exec is not supported by the AWS Fargate provider")
}

// AttachToContainer attaches to the executing process of a container in the pod.
func (p *FargateProvider) AttachToContainer(ctx context.Context, namespace, podName, containerName string, attach api.AttachIO) error {
	return errdefs.InvalidInput("attach is not supported by the AWS Fargate provider")
}

// PortForward forwards a local port to a port on the pod.
func (p *FargateProvider) PortForward(ctx context.Context, namespace, pod string, port int32, stream io.ReadWriteCloser) error {
	return errdefs.InvalidInput("port forwarding is not supported by the AWS Fargate provider")
}

// GetStatsSummary returns the node's stats summary. Fargate does not report per-pod resource usage to the provider, so
// the summary names the node and holds no pods.
func (p *FargateProvider) GetStatsSummary(ctx context.Context) (*statsv1alpha1.Summary, error) {
	return &statsv1alpha1.Summary{Node: statsv1alpha1.NodeStats{NodeName: p.nodeName}}, nil
}

// GetMetricsResource returns the node's resource metrics, of which the provider has none.
func (p *FargateProvider) GetMetricsResource(ctx context.Context) ([]*dto.MetricFamily, error) {
	return nil, nil
}

// GetPodStatus retrieves the status of a pod by name from the provider.
func (p *FargateProvider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	log.Printf("Received GetPodStatus request for %s/%s.\n", namespace, name)

	pod, err := p.cluster.GetPod(namespace, name)
	if err != nil {
		log.Printf("Failed to get pod: %v.\n", err)
		return nil, err
	}

	status := pod.GetStatus(ctx)

	log.Printf("Responding to GetPodStatus: %+v.\n", status)

	return &status, nil
}

// GetPods retrieves a list of all pods running on the provider (can be cached).
func (p *FargateProvider) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
	log.Println("Received GetPods request.")

	pods, err := p.cluster.GetPods()
	if err != nil {
		log.Printf("Failed to get pods: %v.\n", err)
		return nil, err
	}

	var result []*corev1.Pod
	var podNames []string

	for _, pod := range pods {
		spec, err := pod.GetSpec(ctx)
		if err != nil {
			log.Printf("Failed to get pod spec: %v.\n", err)
			continue
		}

		result = append(result, spec)
		podNames = append(podNames, fmt.Sprintf("%s/%s", spec.Namespace, spec.Name))
	}

	log.Printf("Responding to GetPods: %+v.\n", podNames)

	return result, nil
}

// capacityList returns the capacity the provider advertises for the node.
func (p *FargateProvider) capacityList() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:     resource.MustParse(p.capacity.cpu),
		corev1.ResourceMemory:  resource.MustParse(p.capacity.memory),
		corev1.ResourceStorage: resource.MustParse(p.capacity.storage),
		corev1.ResourcePods:    resource.MustParse(p.capacity.pods),
	}
}

// ConfigureNode sets the node's capacity, address, kubelet endpoint and operating system before it is registered.
// The node's conditions are kept by Virtual Kubelet's node controller.
func (p *FargateProvider) ConfigureNode(ctx context.Context, node *corev1.Node) {
	node.Status.Capacity = p.capacityList()
	node.Status.Allocatable = p.capacityList()
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: p.internalIP}}
	node.Status.DaemonEndpoints = corev1.NodeDaemonEndpoints{KubeletEndpoint: corev1.DaemonEndpoint{Port: p.daemonEndpointPort}}
	node.Status.NodeInfo.OperatingSystem = p.operatingSystem
	node.Status.NodeInfo.Architecture = "amd64"
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Labels[corev1.LabelOSStable] = "linux"
	node.Labels[corev1.LabelArchStable] = "amd64"
	node.Labels["node.kubernetes.io/exclude-from-external-load-balancers"] = "true"
}
