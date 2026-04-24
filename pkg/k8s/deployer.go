package k8s

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/chris/kqueue/pkg/api"
	"github.com/chris/kqueue/pkg/scaler"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Deployer manages worker deployments in Kubernetes
type Deployer struct {
	client       kubernetes.Interface
	namespace    string
	natsURL      string
	sidecarImage string
	logger       *slog.Logger
}

// Ensure Deployer implements WorkerScaler
var _ scaler.WorkerScaler = (*Deployer)(nil)

// NewDeployer creates a new Kubernetes deployer
func NewDeployer(namespace, natsURL, sidecarImage string, logger *slog.Logger) (*Deployer, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
		if err != nil {
			return nil, fmt.Errorf("building k8s config: %w", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating k8s client: %w", err)
	}

	return &Deployer{
		client:       clientset,
		namespace:    namespace,
		natsURL:      natsURL,
		sidecarImage: sidecarImage,
		logger:       logger,
	}, nil
}

// NewDeployerWithClient creates a deployer with a provided client (for testing)
func NewDeployerWithClient(client kubernetes.Interface, namespace, natsURL, sidecarImage string, logger *slog.Logger) *Deployer {
	return &Deployer{
		client:       client,
		namespace:    namespace,
		natsURL:      natsURL,
		sidecarImage: sidecarImage,
		logger:       logger,
	}
}

// EnsureDeployment creates or updates a worker deployment for a queue
func (d *Deployer) EnsureDeployment(ctx context.Context, cfg api.QueueConfig) error {
	deployName := "kqueue-worker-" + cfg.Name
	replicas := int32(cfg.Replicas.Min)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: d.namespace,
			Labels: map[string]string{
				"app":          "kqueue-worker",
				"kqueue/queue": cfg.Name,
				"managed-by":   "kqueue",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":          "kqueue-worker",
					"kqueue/queue": cfg.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":          "kqueue-worker",
						"kqueue/queue": cfg.Name,
						"managed-by":   "kqueue",
					},
				},
				Spec: corev1.PodSpec{
					RuntimeClassName: runtimeClassPtr(cfg.RuntimeClass),
					Containers: []corev1.Container{
						d.buildWorkerContainer(cfg),
						{
							Name:            "sidecar",
							Image:           d.sidecarImage,
							ImagePullPolicy: d.pullPolicy(cfg),
							Env: []corev1.EnvVar{
								{Name: "NATS_URL", Value: d.natsURL},
								{Name: "QUEUE_NAME", Value: cfg.Name},
								{Name: "QUEUE_SUBJECT", Value: cfg.Subject},
								{Name: "WORKER_URL", Value: "http://localhost:8080/process"},
								{Name: "MAX_RETRIES", Value: fmt.Sprintf("%d", cfg.MaxRetries)},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
					},
				},
			},
		},
	}

	existing, err := d.client.AppsV1().Deployments(d.namespace).Get(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		// Create new
		_, err = d.client.AppsV1().Deployments(d.namespace).Create(ctx, deployment, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating deployment %s: %w", deployName, err)
		}
		d.logger.Info("deployment created", "name", deployName, "replicas", replicas)
		return nil
	}

	// Update existing
	existing.Spec = deployment.Spec
	_, err = d.client.AppsV1().Deployments(d.namespace).Update(ctx, existing, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating deployment %s: %w", deployName, err)
	}
	d.logger.Info("deployment updated", "name", deployName)
	return nil
}

// Scale adjusts the replica count for a queue's workers
func (d *Deployer) Scale(ctx context.Context, queueName string, replicas int) error {
	deployName := "kqueue-worker-" + queueName
	scale, err := d.client.AppsV1().Deployments(d.namespace).GetScale(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting scale for %s: %w", deployName, err)
	}

	scale.Spec.Replicas = int32(replicas)
	_, err = d.client.AppsV1().Deployments(d.namespace).UpdateScale(ctx, deployName, scale, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("scaling %s to %d: %w", deployName, replicas, err)
	}

	d.logger.Info("scaled deployment", "name", deployName, "replicas", replicas)
	return nil
}

// GetCurrentReplicas returns the current replica count
func (d *Deployer) GetCurrentReplicas(ctx context.Context, queueName string) (int, error) {
	deployName := "kqueue-worker-" + queueName
	deploy, err := d.client.AppsV1().Deployments(d.namespace).Get(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("getting deployment %s: %w", deployName, err)
	}
	if deploy.Spec.Replicas == nil {
		return 1, nil
	}
	return int(*deploy.Spec.Replicas), nil
}

func (d *Deployer) buildWorkerContainer(cfg api.QueueConfig) corev1.Container {
	c := corev1.Container{
		Name:            "worker",
		Image:           cfg.WorkerImage,
		ImagePullPolicy: d.pullPolicy(cfg),
		Ports: []corev1.ContainerPort{
			{ContainerPort: 8080, Name: "http"},
		},
		Resources: d.buildResources(cfg.Resources),
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/health",
					Port: intstr.FromInt(8080),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
	}

	// Add extra env vars from config
	for k, v := range cfg.WorkerEnv {
		c.Env = append(c.Env, corev1.EnvVar{Name: k, Value: v})
	}

	return c
}

func (d *Deployer) pullPolicy(cfg api.QueueConfig) corev1.PullPolicy {
	switch cfg.ImagePullPolicy {
	case "Always":
		return corev1.PullAlways
	case "Never":
		return corev1.PullNever
	case "IfNotPresent":
		return corev1.PullIfNotPresent
	default:
		return corev1.PullIfNotPresent
	}
}

func (d *Deployer) buildResources(cfg api.ResourceConfig) corev1.ResourceRequirements {
	reqs := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	if cfg.CPURequest != "" {
		reqs.Requests[corev1.ResourceCPU] = resource.MustParse(cfg.CPURequest)
	}
	if cfg.CPULimit != "" {
		reqs.Limits[corev1.ResourceCPU] = resource.MustParse(cfg.CPULimit)
	}
	if cfg.MemoryRequest != "" {
		reqs.Requests[corev1.ResourceMemory] = resource.MustParse(cfg.MemoryRequest)
	}
	if cfg.MemoryLimit != "" {
		reqs.Limits[corev1.ResourceMemory] = resource.MustParse(cfg.MemoryLimit)
	}
	if cfg.GPULimit != "" {
		reqs.Limits["nvidia.com/gpu"] = resource.MustParse(cfg.GPULimit)
	}
	return reqs
}

// runtimeClassPtr returns a pointer to the runtime class name, or nil if empty.
func runtimeClassPtr(name string) *string {
	if name == "" {
		return nil
	}
	return &name
}
