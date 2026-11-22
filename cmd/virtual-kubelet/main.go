// Copyright © 2017 The virtual-kubelet authors
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

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	vkaws "github.com/virtual-kubelet/aws-fargate"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	logruslogger "github.com/virtual-kubelet/virtual-kubelet/log/logrus"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/apiserver/pkg/server/options"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

var (
	buildVersion = "N/A"
	buildTime    = "N/A"
	k8sVersion   = "v1.37.1" // This follows the version of the k8s.io libraries the provider is built with.
)

// options holds the command line and environment settings.
type opts struct {
	kubeConfigPath  string
	nodeName        string
	operatingSystem string
	providerConfig  string
	listenAddress   string
	internalIP      string
	logLevel        string
	startupTimeout  time.Duration
	resync          time.Duration
	podSyncWorkers  int

	disableTaint bool
	taintKey     string
	taintValue   string
	taintEffect  string

	certPath       string
	keyPath        string
	clientCACert   string
	clientNoVerify bool

	webhookAuth                  bool
	webhookAuthnCacheTTL         time.Duration
	webhookAuthzAuthedCacheTTL   time.Duration
	webhookAuthzUnauthedCacheTTL time.Duration

	// Accepted and ignored, so that existing deployments keep starting.
	deprecatedNamespace   string
	deprecatedProvider    string
	deprecatedMetricsAddr string
	deprecatedNodeLease   bool
	deprecatedTraceFlags  []string
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	o := defaultOpts()
	binaryName := filepath.Base(os.Args[0])
	desc := binaryName + " implements a node on a Kubernetes cluster that runs its pods as AWS Fargate tasks."

	cmd := &cobra.Command{
		Use:   binaryName,
		Short: desc,
		Long:  desc,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := logrus.StandardLogger()
			level, err := logrus.ParseLevel(o.logLevel)
			if err != nil {
				return fmt.Errorf("invalid log level %q: %w", o.logLevel, err)
			}
			logger.SetLevel(level)
			ctx := log.WithLogger(cmd.Context(), logruslogger.FromLogrus(logrus.NewEntry(logger)))

			if err := run(ctx, o); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
	installFlags(cmd, o)
	cmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Show the version of the program",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("Version: %s, Built: %s\n", buildVersion, buildTime)
		},
	})

	if err := cmd.ExecuteContext(ctx); err != nil {
		logrus.WithError(err).Fatal("Error running command")
	}
}

func defaultOpts() *opts {
	o := &opts{
		kubeConfigPath:  os.Getenv("KUBECONFIG"),
		nodeName:        "virtual-kubelet",
		operatingSystem: "Linux",
		listenAddress:   ":10250",
		internalIP:      os.Getenv("VKUBELET_POD_IP"),
		logLevel:        "info",
		startupTimeout:  0,
		resync:          time.Minute,
		podSyncWorkers:  10,
		taintKey:        envOrDefault("VKUBELET_TAINT_KEY", "virtual-kubelet.io/provider"),
		taintValue:      envOrDefault("VKUBELET_TAINT_VALUE", "aws"),
		taintEffect:     envOrDefault("VKUBELET_TAINT_EFFECT", string(corev1.TaintEffectNoSchedule)),
		certPath:        os.Getenv("APISERVER_CERT_LOCATION"),
		keyPath:         os.Getenv("APISERVER_KEY_LOCATION"),
		clientCACert:    os.Getenv("APISERVER_CA_CERT_LOCATION"),
	}
	if port := os.Getenv("KUBELET_PORT"); port != "" {
		o.listenAddress = ":" + port
	}
	if o.kubeConfigPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path := filepath.Join(home, ".kube", "config")
			if _, err := os.Stat(path); err == nil {
				o.kubeConfigPath = path
			}
		}
	}
	return o
}

func installFlags(cmd *cobra.Command, o *opts) {
	flags := cmd.Flags()

	klogFlags := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(klogFlags)
	klogFlags.VisitAll(func(f *flag.Flag) {
		f.Name = "klog." + f.Name
		flags.AddGoFlag(f)
	})

	flags.StringVar(&o.kubeConfigPath, "kubeconfig", o.kubeConfigPath, "kube config file to use for connecting to the Kubernetes API server; in-cluster configuration is used when empty")
	flags.StringVar(&o.nodeName, "nodename", o.nodeName, "kubernetes node name")
	flags.StringVar(&o.operatingSystem, "os", o.operatingSystem, "operating system of the node (only Linux is supported)")
	flags.StringVar(&o.providerConfig, "provider-config", o.providerConfig, "AWS Fargate provider configuration file")
	flags.StringVar(&o.listenAddress, "listen-address", o.listenAddress, "address the kubelet API listens on (KUBELET_PORT sets the port)")
	flags.StringVar(&o.logLevel, "log-level", o.logLevel, "log level")
	flags.DurationVar(&o.startupTimeout, "startup-timeout", o.startupTimeout, "how long to wait for the virtual-kubelet to start")
	flags.DurationVar(&o.resync, "full-resync-period", o.resync, "how often to perform a full resync of pods between kubernetes and the provider")
	flags.IntVar(&o.podSyncWorkers, "pod-sync-workers", o.podSyncWorkers, "number of pod synchronization workers")
	flags.BoolVar(&o.disableTaint, "disable-taint", o.disableTaint, "do not taint the node (VKUBELET_TAINT_KEY, VKUBELET_TAINT_VALUE and VKUBELET_TAINT_EFFECT set the taint)")

	flags.StringVar(&o.clientCACert, "client-verify-ca", o.clientCACert, "CA certificate to verify client certificates with (APISERVER_CA_CERT_LOCATION)")
	flags.BoolVar(&o.clientNoVerify, "no-verify-clients", o.clientNoVerify, "do not require client certificates")
	flags.BoolVar(&o.webhookAuth, "authentication-token-webhook", o.webhookAuth, "use the TokenReview and SubjectAccessReview APIs to authenticate and authorize requests to the kubelet API")
	flags.DurationVar(&o.webhookAuthnCacheTTL, "authentication-token-webhook-cache-ttl", o.webhookAuthnCacheTTL, "duration to cache responses from the webhook token authenticator")
	flags.DurationVar(&o.webhookAuthzAuthedCacheTTL, "authorization-webhook-cache-authorized-ttl", o.webhookAuthzAuthedCacheTTL, "duration to cache 'authorized' responses from the webhook authorizer")
	flags.DurationVar(&o.webhookAuthzUnauthedCacheTTL, "authorization-webhook-cache-unauthorized-ttl", o.webhookAuthzUnauthedCacheTTL, "duration to cache 'unauthorized' responses from the webhook authorizer")

	deprecated := func(name, message string) {
		_ = flags.MarkDeprecated(name, message)
		_ = flags.MarkHidden(name)
	}
	flags.StringVar(&o.deprecatedNamespace, "namespace", "", "")
	deprecated("namespace", "all namespaces are watched")
	flags.StringVar(&o.deprecatedProvider, "provider", "", "")
	deprecated("provider", "aws is the only provider")
	flags.StringVar(&o.deprecatedMetricsAddr, "metrics-addr", "", "")
	deprecated("metrics-addr", "metrics are served on the kubelet API's address")
	flags.BoolVar(&o.deprecatedNodeLease, "enable-node-lease", false, "")
	deprecated("enable-node-lease", "node leases are always used")
	flags.StringVar(&o.taintKey, "taint", o.taintKey, "")
	deprecated("taint", "set the taint key with VKUBELET_TAINT_KEY")
	for _, name := range []string{"trace-exporter", "trace-service-name", "trace-tag", "trace-sample-rate"} {
		flags.StringSliceVar(&o.deprecatedTraceFlags, name, nil, "")
		deprecated(name, "tracing was removed")
	}
}

func run(ctx context.Context, o *opts) error {
	client, err := nodeutil.ClientsetFromEnv(o.kubeConfigPath)
	if err != nil {
		return err
	}

	vk, err := newNode(ctx, o, client)
	if err != nil {
		return err
	}

	go func() {
		if err := vk.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.G(ctx).WithError(err).Error("the node stopped")
		}
	}()

	if err := vk.WaitReady(ctx, o.startupTimeout); err != nil {
		return fmt.Errorf("error waiting for the node to be ready: %w", err)
	}
	log.G(ctx).Info("the node is ready")

	<-vk.Done()
	return vk.Err()
}

// newNode makes the virtual node and its provider, as the command runs them, with the given Kubernetes client.
func newNode(ctx context.Context, o *opts, client kubernetes.Interface) (*nodeutil.Node, error) {
	if o.providerConfig == "" {
		return nil, errors.New("--provider-config is required")
	}
	_, portText, err := net.SplitHostPort(o.listenAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid --listen-address %q: %w", o.listenAddress, err)
	}
	var port int32
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil {
		return nil, fmt.Errorf("invalid port in --listen-address %q: %w", o.listenAddress, err)
	}
	serveAPI := o.certPath != "" && o.keyPath != ""
	if !serveAPI {
		if o.webhookAuth {
			return nil, errors.New("--authentication-token-webhook needs APISERVER_CERT_LOCATION and APISERVER_KEY_LOCATION")
		}
		log.G(ctx).Warn("APISERVER_CERT_LOCATION and APISERVER_KEY_LOCATION are not set: the kubelet API (logs) is not served")
	}

	withTaint := func(cfg *nodeutil.NodeConfig) error {
		if o.disableTaint {
			return nil
		}
		taint := corev1.Taint{Key: o.taintKey, Value: o.taintValue}
		switch o.taintEffect {
		case "NoSchedule":
			taint.Effect = corev1.TaintEffectNoSchedule
		case "NoExecute":
			taint.Effect = corev1.TaintEffectNoExecute
		case "PreferNoSchedule":
			taint.Effect = corev1.TaintEffectPreferNoSchedule
		default:
			return errdefs.InvalidInputf("taint effect %q is not supported", o.taintEffect)
		}
		cfg.NodeSpec.Spec.Taints = append(cfg.NodeSpec.Spec.Taints, taint)
		return nil
	}
	withVersion := func(cfg *nodeutil.NodeConfig) error {
		cfg.NodeSpec.Status.NodeInfo.KubeletVersion = strings.Join([]string{k8sVersion, "vk-aws-fargate", buildVersion}, "-")
		return nil
	}
	configureRoutes := func(cfg *nodeutil.NodeConfig) error {
		mux := http.NewServeMux()
		cfg.Handler = mux
		return nodeutil.AttachProviderRoutes(mux)(cfg)
	}
	withAuth := func(cfg *nodeutil.NodeConfig) error {
		if !o.webhookAuth {
			cfg.Handler = api.InstrumentHandler(nodeutil.WithAuth(nodeutil.NoAuth(), cfg.Handler))
			return nil
		}
		auth, err := nodeutil.WebhookAuth(cfg.Client, o.nodeName, func(wcfg *nodeutil.WebhookAuthConfig) error {
			wcfg.AuthzConfig.WebhookRetryBackoff = options.DefaultAuthWebhookRetryBackoff()
			if o.webhookAuthnCacheTTL > 0 {
				wcfg.AuthnConfig.CacheTTL = o.webhookAuthnCacheTTL
			}
			if o.webhookAuthzAuthedCacheTTL > 0 {
				wcfg.AuthzConfig.AllowCacheTTL = o.webhookAuthzAuthedCacheTTL
			}
			if o.webhookAuthzUnauthedCacheTTL > 0 {
				wcfg.AuthzConfig.DenyCacheTTL = o.webhookAuthzUnauthedCacheTTL
			}
			if o.clientCACert != "" {
				ca, err := dynamiccertificates.NewDynamicCAContentFromFile("client-ca", o.clientCACert)
				if err != nil {
					return err
				}
				wcfg.AuthnConfig.ClientCertificateCAContentProvider = ca
				go ca.Run(ctx, 1)
			}
			return nil
		})
		if err != nil {
			return err
		}
		cfg.TLSConfig.ClientAuth = tls.RequestClientCert
		cfg.Handler = api.InstrumentHandler(nodeutil.WithAuth(auth, cfg.Handler))
		return nil
	}
	withCA := func(cfg *tls.Config) error {
		if o.clientCACert == "" {
			return nil
		}
		if err := nodeutil.WithCAFromPath(o.clientCACert)(cfg); err != nil {
			return fmt.Errorf("error reading the client CA: %w", err)
		}
		if o.clientNoVerify {
			cfg.ClientAuth = tls.NoClientCert
		}
		return nil
	}

	nodeOpts := []nodeutil.NodeOpt{
		nodeutil.WithClient(client),
		withTaint,
		withVersion,
		func(cfg *nodeutil.NodeConfig) error {
			cfg.InformerResyncPeriod = o.resync
			cfg.NumWorkers = o.podSyncWorkers
			cfg.HTTPListenAddr = o.listenAddress
			return nil
		},
	}
	if serveAPI {
		nodeOpts = append(nodeOpts,
			nodeutil.WithTLSConfig(nodeutil.WithKeyPairFromPath(o.certPath, o.keyPath), withCA),
			configureRoutes,
			withAuth,
		)
	}

	return nodeutil.NewNode(o.nodeName,
		func(pc nodeutil.ProviderConfig) (nodeutil.Provider, node.NodeProvider, error) {
			p, err := vkaws.NewFargateProvider(ctx, o.providerConfig, o.nodeName, o.operatingSystem, o.internalIP, port)
			if err != nil {
				return nil, nil, err
			}
			p.ConfigureNode(ctx, pc.Node)
			p.UsePodLister(pc.Pods)
			return p, nil, nil
		},
		nodeOpts...,
	)
}

func envOrDefault(key string, defaultValue string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return defaultValue
}
