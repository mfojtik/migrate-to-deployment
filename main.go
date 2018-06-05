package main

import (
	goflag "flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"time"

	color "github.com/logrusorgru/aurora"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"

	osappsv1 "github.com/openshift/api/apps/v1"
	osappsv1client "github.com/openshift/client-go/apps/clientset/versioned/typed/apps/v1"
	"github.com/openshift/library-go/pkg/serviceability"
)

func main() {
	rand.Seed(time.Now().UTC().UnixNano())
	pflag.CommandLine.AddGoFlagSet(goflag.CommandLine)
	defer serviceability.Profile(os.Getenv("OPENSHIFT_PROFILE")).Stop()

	if len(os.Getenv("GOMAXPROCS")) == 0 {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	command := NewMigrateCommand(os.Stdout)
	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

type MigrateOptions struct {
	Output io.Writer

	DeploymentConfigNames []string
	Namespace             string

	OsAppsClient osappsv1client.AppsV1Interface
	AppsClient   appsv1client.AppsV1Interface
	CoreClient   corev1client.CoreV1Interface

	kubeconfig string

	convert        func(*osappsv1.DeploymentConfig, *appsv1.Deployment) error
	migrateHistory func(*appsv1.Deployment, []corev1.ReplicationController) error
}

func (m *MigrateOptions) Validate(c *cobra.Command) error {
	if len(os.Args) == 1 {
		return fmt.Errorf("deployment config name(s) must be specified\n")
	}
	m.DeploymentConfigNames = os.Args[1:]
	return nil
}

func (m *MigrateOptions) Complete(c *cobra.Command) error {
	config, err := clientcmd.BuildConfigFromFlags("", m.kubeconfig)
	if err != nil {
		return err
	}

	m.AppsClient, err = appsv1client.NewForConfig(config)
	if err != nil {
		return err
	}

	m.OsAppsClient, err = osappsv1client.NewForConfig(config)
	if err != nil {
		return err
	}

	m.CoreClient, err = corev1client.NewForConfig(config)
	if err != nil {
		return err
	}

	return nil
}

func (m *MigrateOptions) progress(message string) {
	fmt.Fprintf(m.Output, color.Bold("-->").String()+" "+message+"\n")
}

func (m *MigrateOptions) Run() error {
	for _, name := range m.DeploymentConfigNames {
		m.progress(fmt.Sprintf("processing deployment config %q ...", color.Blue(m.Namespace+"/"+name)))
		dc, err := m.OsAppsClient.DeploymentConfigs(m.Namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		m.progress(fmt.Sprintf("pausing deployment config %q ...", color.Blue(dc.Namespace+"/"+dc.Name)))
		dc.Spec.Paused = true
		_, err = m.OsAppsClient.DeploymentConfigs(m.Namespace).Update(dc)
		if err != nil {
			return err
		}

		var deployment *appsv1.Deployment

		m.progress(fmt.Sprintf("converting deployment config %q to kubernetes deployment...", color.Blue(dc.Namespace+"/"+dc.Name)))
		err = m.convert(dc, deployment)
		if err != nil {
			return err
		}

		// Pause deployment so we can finish transition
		deployment.Spec.Paused = true

		m.progress(fmt.Sprintf("creating paused deployment %q ...", color.Blue(deployment.Namespace+"/"+deployment.Name)))
		newDeployment, err := m.AppsClient.Deployments(m.Namespace).Create(deployment)
		if err != nil {
			return err
		}

		// TODO: Move this to openshift/api
		selector := labels.SelectorFromValidatedSet(labels.Set{"openshift.io/deployment-config.name": dc.Name})
		rcs, err := m.CoreClient.ReplicationControllers(m.Namespace).List(metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return err
		}

		if len(rcs.Items) > 0 {
			m.progress(fmt.Sprintf("found %d replication controllers managed by %q:", len(rcs.Items),
				color.Blue(deployment.Namespace+"/"+deployment.Name)))
			for _, rc := range rcs.Items {
				m.progress(fmt.Sprintf("  --> %s", color.Gray(rc.Name)))
			}
		}

		err = m.migrateHistory(newDeployment, rcs.Items)
		if err != nil {
			return err
		}
	}
	return nil
}

func NewMigrateCommand(out io.Writer) *cobra.Command {
	options := &MigrateOptions{Output: out}

	cmd := &cobra.Command{
		Use:   "migrate-to-deployment",
		Short: "This command migrate your deployment config to kubernetes deployment",
		Run: func(cmd *cobra.Command, args []string) {
			if err := options.Validate(cmd); err != nil {
				fmt.Fprintf(os.Stderr, color.Red("ERROR:").String()+" %v\n", err)
				os.Exit(1)
			}
			if err := options.Complete(cmd); err != nil {
				fmt.Fprintf(os.Stderr, color.Red("ERROR:").String()+" %v\n", err)
				os.Exit(1)
			}
			if err := options.Run(); err != nil {
				fmt.Fprintf(os.Stderr, color.Red("ERROR:").String()+" %v\n", err)
				os.Exit(1)
			}
		},
	}

	if home := homeDir(); len(home) > 0 {
		cmd.Flags().StringVar(&options.kubeconfig, "kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		cmd.Flags().StringVar(&options.kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	}

	cmd.Flags().StringVarP(&options.Namespace, "namespace", "n", "", "namespace to use (default: current namespace)")

	cmd.SetUsageFunc(func(c *cobra.Command) error {
		fmt.Fprintf(os.Stderr, "Usage: %s dc/foo dc/ba\nr", c.Name())
		return nil
	})

	return cmd
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}
