package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/ipfs/testground/pkg/api"
	"github.com/ipfs/testground/pkg/logging"
	"github.com/ipfs/testground/pkg/util"
	"github.com/ipfs/testground/sdk/runtime"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	_ api.Runner = &ClusterK8sRunner{}
)

func homeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	if usr, err := user.Current(); err == nil {
		return usr.HomeDir
	}
	return ""
}

// ClusterK8sRunnerConfig is the configuration object of this runner. Boolean
// values are expressed in a way that zero value (false) is the default setting.
type ClusterK8sRunnerConfig struct {
	// LogLevel sets the log level in the test containers (default: not set).
	LogLevel string `toml:"log_level"`

	// Background avoids tailing the output of containers, and displaying it as
	// log messages (default: true).
	Background bool `toml:"background"`
}

// ClusterK8sRunner is a runner that creates a Docker service to launch as
// many replicated instances of a container as the run job indicates.
type ClusterK8sRunner struct{}

type KubernetesConfig struct {
	// KubeConfigPath is the path to your kubernetes configuration path
	KubeConfigPath string `json:"kubeConfigPath"`
	// Namespace is the kubernetes namespaces where the pods should be running
	Namespace string `json:"namespace"`
}

// defaultKubernetesConfig uses the default ~/.kube/config
// to discover the kubernetes clusters. It also uses the "default" namespace.
func defaultKubernetesConfig() KubernetesConfig {
	kubeconfig := filepath.Join(homeDir(), ".kube", "config")
	return KubernetesConfig{
		KubeConfigPath: kubeconfig,
		Namespace:      "default",
	}
}

// TODO runner option to keep containers alive instead of deleting them after
// the test has run.
func (*ClusterK8sRunner) Run(input *api.RunInput, ow io.Writer) (*api.RunOutput, error) {
	var (
		image = input.ArtifactPath
		seq   = input.Seq
		log   = logging.S().With("runner", "cluster:k8s", "run_id", input.RunID)
		cfg   = *input.RunnerConfig.(*ClusterK8sRunnerConfig)
	)

	// Sanity check.
	if seq < 0 || seq >= len(input.TestPlan.TestCases) {
		return nil, fmt.Errorf("invalid test case seq %d for plan %s", seq, input.TestPlan.Name)
	}

	// Get the test case.
	testcase := input.TestPlan.TestCases[seq]

	// Build a runenv.
	runenv := &runtime.RunEnv{
		TestPlan:           input.TestPlan.Name,
		TestCase:           testcase.Name,
		TestRun:            input.RunID,
		TestCaseSeq:        seq,
		TestInstanceCount:  input.Instances,
		TestInstanceParams: input.Parameters,
		TestSidecar:        true,
	}

	// Serialize the runenv into env variables to pass to docker.
	//env := util.ToOptionsSlice(runenv.ToEnvVars())

	env := util.ToEnvVar(runenv.ToEnvVars())

	// Define k8s client configuration
	config := defaultKubernetesConfig()
	k8scfg, err := clientcmd.BuildConfigFromFlags("", config.KubeConfigPath)
	if err != nil {
		return nil, fmt.Errorf("could not start k8s client from config: %v", err)
	}

	// Create the clientset
	clientset, err := kubernetes.NewForConfig(k8scfg)
	if err != nil {
		return nil, fmt.Errorf("could not create clientset: %v", err)
	}

	var (
		sname    = fmt.Sprintf("tg-%s-%s-%s", input.TestPlan.Name, testcase.Name, input.RunID)
		replicas = uint64(input.Instances)
	)

	log.Infow("creating k8s deployment", "name", sname, "image", image, "replicas", replicas)

	var wg sync.WaitGroup
	wg.Add(int(replicas))

	for i := uint64(1); i <= replicas; i++ {
		i := i
		go func() {
			defer wg.Done()

			podName := fmt.Sprintf("tg-%s-%d", input.TestPlan.Name, i)

			// Create Kubernetes Pod
			podRequest := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: podName,
					Labels: map[string]string{
						"testground.plan":     input.TestPlan.Name,
						"testground.testcase": testcase.Name,
						"testground.runid":    input.RunID,
					},
				},
				Spec: v1.PodSpec{
					RestartPolicy: "Never",
					Containers: []v1.Container{
						{
							Name:  podName,
							Image: image,
							Args:  []string{},
							Env:   env,
							Resources: v1.ResourceRequirements{
								Limits: v1.ResourceList{
									v1.ResourceMemory: resource.MustParse("30Mi"),
								},
							},
						},
					},
				},
			}
			_, err := clientset.CoreV1().Pods(config.Namespace).Create(podRequest)
			if err != nil {
				return
			}

			// Wait for pod
			start := time.Now()
			for {
				log.Debugw("Waiting for pod", "pod", podName)
				pod, err := clientset.CoreV1().Pods(config.Namespace).Get(podName, metav1.GetOptions{})
				if err != nil {
					time.Sleep(100 * time.Millisecond)
					continue
				}
				if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
					break
				}
				if time.Since(start) > 5*time.Minute {
					return
				}
				time.Sleep(2000 * time.Millisecond)
			}
		}()
	}

	wg.Wait()

	defer func() {
		if cfg.KeepService {
			log.Info("skipping removing the pods due to user request")
			return
		}
		err = retry(5, 1*time.Second, func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			return cli.NetworkRemove(ctx, networkID)
		})
		if err != nil {
			log.Errorw("couldn't remove network", "network", networkID, "err", err)
		}
	}()

	out := &api.RunOutput{RunnerID: "smth"}
	return out, nil
}

func (*ClusterK8sRunner) ID() string {
	return "cluster:k8s"
}

func (*ClusterK8sRunner) ConfigType() reflect.Type {
	return reflect.TypeOf(ClusterK8sRunnerConfig{})
}

func (*ClusterK8sRunner) CompatibleBuilders() []string {
	return []string{"docker:go"}
}

func int32Ptr(i int32) *int32 { return &i }
