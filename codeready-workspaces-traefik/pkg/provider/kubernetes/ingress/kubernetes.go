package ingress

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/mitchellh/hashstructure"
	ptypes "github.com/traefik/paerser/types"
	"github.com/traefik/traefik/v2/pkg/config/dynamic"
	"github.com/traefik/traefik/v2/pkg/job"
	"github.com/traefik/traefik/v2/pkg/log"
	"github.com/traefik/traefik/v2/pkg/provider"
	"github.com/traefik/traefik/v2/pkg/safe"
	"github.com/traefik/traefik/v2/pkg/tls"
	corev1 "k8s.io/api/core/v1"
	networkingv1beta1 "k8s.io/api/networking/v1beta1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	annotationKubernetesIngressClass     = "kubernetes.io/ingress.class"
	traefikDefaultIngressClass           = "traefik"
	traefikDefaultIngressClassController = "traefik.io/ingress-controller"
	defaultPathMatcher                   = "PathPrefix"
)

// Provider holds configurations of the provider.
type Provider struct {
	Endpoint               string           `description:"Kubernetes server endpoint (required for external cluster client)." json:"endpoint,omitempty" toml:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	Token                  string           `description:"Kubernetes bearer token (not needed for in-cluster client)." json:"token,omitempty" toml:"token,omitempty" yaml:"token,omitempty"`
	CertAuthFilePath       string           `description:"Kubernetes certificate authority file path (not needed for in-cluster client)." json:"certAuthFilePath,omitempty" toml:"certAuthFilePath,omitempty" yaml:"certAuthFilePath,omitempty"`
	DisablePassHostHeaders bool             `description:"Kubernetes disable PassHost Headers." json:"disablePassHostHeaders,omitempty" toml:"disablePassHostHeaders,omitempty" yaml:"disablePassHostHeaders,omitempty" export:"true"`
	Namespaces             []string         `description:"Kubernetes namespaces." json:"namespaces,omitempty" toml:"namespaces,omitempty" yaml:"namespaces,omitempty" export:"true"`
	LabelSelector          string           `description:"Kubernetes Ingress label selector to use." json:"labelSelector,omitempty" toml:"labelSelector,omitempty" yaml:"labelSelector,omitempty" export:"true"`
	IngressClass           string           `description:"Value of kubernetes.io/ingress.class annotation to watch for." json:"ingressClass,omitempty" toml:"ingressClass,omitempty" yaml:"ingressClass,omitempty" export:"true"`
	IngressEndpoint        *EndpointIngress `description:"Kubernetes Ingress Endpoint." json:"ingressEndpoint,omitempty" toml:"ingressEndpoint,omitempty" yaml:"ingressEndpoint,omitempty"`
	ThrottleDuration       ptypes.Duration  `description:"Ingress refresh throttle duration" json:"throttleDuration,omitempty" toml:"throttleDuration,omitempty" yaml:"throttleDuration,omitempty"`
	lastConfiguration      safe.Safe
}

// EndpointIngress holds the endpoint information for the Kubernetes provider.
type EndpointIngress struct {
	IP               string `description:"IP used for Kubernetes Ingress endpoints." json:"ip,omitempty" toml:"ip,omitempty" yaml:"ip,omitempty"`
	Hostname         string `description:"Hostname used for Kubernetes Ingress endpoints." json:"hostname,omitempty" toml:"hostname,omitempty" yaml:"hostname,omitempty"`
	PublishedService string `description:"Published Kubernetes Service to copy status from." json:"publishedService,omitempty" toml:"publishedService,omitempty" yaml:"publishedService,omitempty"`
}

func (p *Provider) newK8sClient(ctx context.Context, ingressLabelSelector string) (*clientWrapper, error) {
	ingLabelSel, err := labels.Parse(ingressLabelSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid ingress label selector: %q", ingressLabelSelector)
	}

	logger := log.FromContext(ctx)

	logger.Infof("ingress label selector is: %q", ingLabelSel)

	withEndpoint := ""
	if p.Endpoint != "" {
		withEndpoint = fmt.Sprintf(" with endpoint %v", p.Endpoint)
	}

	var cl *clientWrapper
	switch {
	case os.Getenv("KUBERNETES_SERVICE_HOST") != "" && os.Getenv("KUBERNETES_SERVICE_PORT") != "":
		logger.Infof("Creating in-cluster Provider client%s", withEndpoint)
		cl, err = newInClusterClient(p.Endpoint)
	case os.Getenv("KUBECONFIG") != "":
		logger.Infof("Creating cluster-external Provider client from KUBECONFIG %s", os.Getenv("KUBECONFIG"))
		cl, err = newExternalClusterClientFromFile(os.Getenv("KUBECONFIG"))
	default:
		logger.Infof("Creating cluster-external Provider client%s", withEndpoint)
		cl, err = newExternalClusterClient(p.Endpoint, p.Token, p.CertAuthFilePath)
	}

	if err == nil {
		cl.ingressLabelSelector = ingLabelSel
	}

	return cl, err
}

// Init the provider.
func (p *Provider) Init() error {
	return nil
}

// Provide allows the k8s provider to provide configurations to traefik
// using the given configuration channel.
func (p *Provider) Provide(configurationChan chan<- dynamic.Message, pool *safe.Pool) error {
	ctxLog := log.With(context.Background(), log.Str(log.ProviderName, "kubernetes"))
	logger := log.FromContext(ctxLog)

	logger.Debugf("Using Ingress label selector: %q", p.LabelSelector)
	k8sClient, err := p.newK8sClient(ctxLog, p.LabelSelector)
	if err != nil {
		return err
	}

	pool.GoCtx(func(ctxPool context.Context) {
		operation := func() error {
			eventsChan, err := k8sClient.WatchAll(p.Namespaces, ctxPool.Done())
			if err != nil {
				logger.Errorf("Error watching kubernetes events: %v", err)
				timer := time.NewTimer(1 * time.Second)
				select {
				case <-timer.C:
					return err
				case <-ctxPool.Done():
					return nil
				}
			}

			throttleDuration := time.Duration(p.ThrottleDuration)
			throttledChan := throttleEvents(ctxLog, throttleDuration, pool, eventsChan)
			if throttledChan != nil {
				eventsChan = throttledChan
			}

			for {
				select {
				case <-ctxPool.Done():
					return nil
				case event := <-eventsChan:
					// Note that event is the *first* event that came in during this
					// throttling interval -- if we're hitting our throttle, we may have
					// dropped events. This is fine, because we don't treat different
					// event types differently. But if we do in the future, we'll need to
					// track more information about the dropped events.
					conf := p.loadConfigurationFromIngresses(ctxLog, k8sClient)

					confHash, err := hashstructure.Hash(conf, nil)
					switch {
					case err != nil:
						logger.Error("Unable to hash the configuration")
					case p.lastConfiguration.Get() == confHash:
						logger.Debugf("Skipping Kubernetes event kind %T", event)
					default:
						p.lastConfiguration.Set(confHash)
						configurationChan <- dynamic.Message{
							ProviderName:  "kubernetes",
							Configuration: conf,
						}
					}

					// If we're throttling, we sleep here for the throttle duration to
					// enforce that we don't refresh faster than our throttle. time.Sleep
					// returns immediately if p.ThrottleDuration is 0 (no throttle).
					time.Sleep(throttleDuration)
				}
			}
		}

		notify := func(err error, time time.Duration) {
			logger.Errorf("Provider connection error: %s; retrying in %s", err, time)
		}

		err := backoff.RetryNotify(safe.OperationWithRecover(operation), backoff.WithContext(job.NewBackOff(backoff.NewExponentialBackOff()), ctxPool), notify)
		if err != nil {
			logger.Errorf("Cannot connect to Provider: %s", err)
		}
	})

	return nil
}

func (p *Provider) loadConfigurationFromIngresses(ctx context.Context, client Client) *dynamic.Configuration {
	conf := &dynamic.Configuration{
		HTTP: &dynamic.HTTPConfiguration{
			Routers:     map[string]*dynamic.Router{},
			Middlewares: map[string]*dynamic.Middleware{},
			Services:    map[string]*dynamic.Service{},
		},
		TCP: &dynamic.TCPConfiguration{},
	}

	serverVersion, err := client.GetServerVersion()
	if err != nil {
		log.FromContext(ctx).Errorf("Failed to get server version: %v", err)
		return conf
	}

	var ingressClass *networkingv1beta1.IngressClass

	if supportsIngressClass(serverVersion) {
		ic, err := client.GetIngressClass()
		if err != nil {
			log.FromContext(ctx).Warnf("Failed to find an ingress class: %v", err)
		}

		ingressClass = ic
	}

	ingresses := client.GetIngresses()

	certConfigs := make(map[string]*tls.CertAndStores)
	for _, ingress := range ingresses {
		ctx = log.With(ctx, log.Str("ingress", ingress.Name), log.Str("namespace", ingress.Namespace))

		if !p.shouldProcessIngress(p.IngressClass, ingress, ingressClass) {
			continue
		}

		rtConfig, err := parseRouterConfig(ingress.Annotations)
		if err != nil {
			log.FromContext(ctx).Errorf("Failed to parse annotations: %v", err)
			continue
		}

		err = getCertificates(ctx, ingress, client, certConfigs)
		if err != nil {
			log.FromContext(ctx).Errorf("Error configuring TLS: %v", err)
		}

		if len(ingress.Spec.Rules) == 0 && ingress.Spec.Backend != nil {
			if _, ok := conf.HTTP.Services["default-backend"]; ok {
				log.FromContext(ctx).Error("The default backend already exists.")
				continue
			}

			service, err := loadService(client, ingress.Namespace, *ingress.Spec.Backend)
			if err != nil {
				log.FromContext(ctx).
					WithField("serviceName", ingress.Spec.Backend.ServiceName).
					WithField("servicePort", ingress.Spec.Backend.ServicePort.String()).
					Errorf("Cannot create service: %v", err)
				continue
			}

			rt := &dynamic.Router{
				Rule:     "PathPrefix(`/`)",
				Priority: math.MinInt32,
				Service:  "default-backend",
			}

			if rtConfig != nil && rtConfig.Router != nil {
				rt.EntryPoints = rtConfig.Router.EntryPoints
				rt.Middlewares = rtConfig.Router.Middlewares
				rt.TLS = rtConfig.Router.TLS
			}

			conf.HTTP.Routers["default-router"] = rt
			conf.HTTP.Services["default-backend"] = service
		}

		for _, rule := range ingress.Spec.Rules {
			if err := p.updateIngressStatus(ingress, client); err != nil {
				log.FromContext(ctx).Errorf("Error while updating ingress status: %v", err)
			}

			if rule.HTTP == nil {
				continue
			}

			for _, pa := range rule.HTTP.Paths {
				service, err := loadService(client, ingress.Namespace, pa.Backend)
				if err != nil {
					log.FromContext(ctx).
						WithField("serviceName", pa.Backend.ServiceName).
						WithField("servicePort", pa.Backend.ServicePort.String()).
						Errorf("Cannot create service: %v", err)
					continue
				}

				serviceName := provider.Normalize(ingress.Namespace + "-" + pa.Backend.ServiceName + "-" + pa.Backend.ServicePort.String())
				conf.HTTP.Services[serviceName] = service

				routerKey := strings.TrimPrefix(provider.Normalize(ingress.Name+"-"+ingress.Namespace+"-"+rule.Host+pa.Path), "-")

				conf.HTTP.Routers[routerKey] = loadRouter(rule, pa, rtConfig, serviceName)
			}
		}
	}

	certs := getTLSConfig(certConfigs)
	if len(certs) > 0 {
		conf.TLS = &dynamic.TLSConfiguration{
			Certificates: certs,
		}
	}

	return conf
}

func (p *Provider) updateIngressStatus(ing *networkingv1beta1.Ingress, k8sClient Client) error {
	// Only process if an EndpointIngress has been configured.
	if p.IngressEndpoint == nil {
		return nil
	}

	if len(p.IngressEndpoint.PublishedService) == 0 {
		if len(p.IngressEndpoint.IP) == 0 && len(p.IngressEndpoint.Hostname) == 0 {
			return errors.New("publishedService or ip or hostname must be defined")
		}

		return k8sClient.UpdateIngressStatus(ing, p.IngressEndpoint.IP, p.IngressEndpoint.Hostname)
	}

	serviceInfo := strings.Split(p.IngressEndpoint.PublishedService, "/")
	if len(serviceInfo) != 2 {
		return fmt.Errorf("invalid publishedService format (expected 'namespace/service' format): %s", p.IngressEndpoint.PublishedService)
	}

	serviceNamespace, serviceName := serviceInfo[0], serviceInfo[1]

	service, exists, err := k8sClient.GetService(serviceNamespace, serviceName)
	if err != nil {
		return fmt.Errorf("cannot get service %s, received error: %w", p.IngressEndpoint.PublishedService, err)
	}

	if exists && service.Status.LoadBalancer.Ingress == nil {
		// service exists, but has no Load Balancer status
		log.Debugf("Skipping updating Ingress %s/%s due to service %s having no status set", ing.Namespace, ing.Name, p.IngressEndpoint.PublishedService)
		return nil
	}

	if !exists {
		return fmt.Errorf("missing service: %s", p.IngressEndpoint.PublishedService)
	}

	return k8sClient.UpdateIngressStatus(ing, service.Status.LoadBalancer.Ingress[0].IP, service.Status.LoadBalancer.Ingress[0].Hostname)
}

func (p *Provider) shouldProcessIngress(providerIngressClass string, ingress *networkingv1beta1.Ingress, ingressClass *networkingv1beta1.IngressClass) bool {
	// configuration through the new kubernetes ingressClass
	if ingress.Spec.IngressClassName != nil {
		return ingressClass != nil && ingressClass.ObjectMeta.Name == *ingress.Spec.IngressClassName
	}

	return providerIngressClass == ingress.Annotations[annotationKubernetesIngressClass] ||
		len(providerIngressClass) == 0 && ingress.Annotations[annotationKubernetesIngressClass] == traefikDefaultIngressClass
}

func buildHostRule(host string) string {
	if strings.HasPrefix(host, "*.") {
		return "HostRegexp(`" + strings.Replace(host, "*.", "{subdomain:[a-zA-Z0-9-]+}.", 1) + "`)"
	}

	return "Host(`" + host + "`)"
}

func getCertificates(ctx context.Context, ingress *networkingv1beta1.Ingress, k8sClient Client, tlsConfigs map[string]*tls.CertAndStores) error {
	for _, t := range ingress.Spec.TLS {
		if t.SecretName == "" {
			log.FromContext(ctx).Debugf("Skipping TLS sub-section: No secret name provided")
			continue
		}

		configKey := ingress.Namespace + "-" + t.SecretName
		if _, tlsExists := tlsConfigs[configKey]; !tlsExists {
			secret, exists, err := k8sClient.GetSecret(ingress.Namespace, t.SecretName)
			if err != nil {
				return fmt.Errorf("failed to fetch secret %s/%s: %w", ingress.Namespace, t.SecretName, err)
			}
			if !exists {
				return fmt.Errorf("secret %s/%s does not exist", ingress.Namespace, t.SecretName)
			}

			cert, key, err := getCertificateBlocks(secret, ingress.Namespace, t.SecretName)
			if err != nil {
				return err
			}

			tlsConfigs[configKey] = &tls.CertAndStores{
				Certificate: tls.Certificate{
					CertFile: tls.FileOrContent(cert),
					KeyFile:  tls.FileOrContent(key),
				},
			}
		}
	}

	return nil
}

func getCertificateBlocks(secret *corev1.Secret, namespace, secretName string) (string, string, error) {
	var missingEntries []string

	tlsCrtData, tlsCrtExists := secret.Data["tls.crt"]
	if !tlsCrtExists {
		missingEntries = append(missingEntries, "tls.crt")
	}

	tlsKeyData, tlsKeyExists := secret.Data["tls.key"]
	if !tlsKeyExists {
		missingEntries = append(missingEntries, "tls.key")
	}

	if len(missingEntries) > 0 {
		return "", "", fmt.Errorf("secret %s/%s is missing the following TLS data entries: %s",
			namespace, secretName, strings.Join(missingEntries, ", "))
	}

	cert := string(tlsCrtData)
	if cert == "" {
		missingEntries = append(missingEntries, "tls.crt")
	}

	key := string(tlsKeyData)
	if key == "" {
		missingEntries = append(missingEntries, "tls.key")
	}

	if len(missingEntries) > 0 {
		return "", "", fmt.Errorf("secret %s/%s contains the following empty TLS data entries: %s",
			namespace, secretName, strings.Join(missingEntries, ", "))
	}

	return cert, key, nil
}

func getTLSConfig(tlsConfigs map[string]*tls.CertAndStores) []*tls.CertAndStores {
	var secretNames []string
	for secretName := range tlsConfigs {
		secretNames = append(secretNames, secretName)
	}
	sort.Strings(secretNames)

	var configs []*tls.CertAndStores
	for _, secretName := range secretNames {
		configs = append(configs, tlsConfigs[secretName])
	}

	return configs
}

func loadService(client Client, namespace string, backend networkingv1beta1.IngressBackend) (*dynamic.Service, error) {
	service, exists, err := client.GetService(namespace, backend.ServiceName)
	if err != nil {
		return nil, err
	}

	if !exists {
		return nil, errors.New("service not found")
	}

	var portName string
	var portSpec corev1.ServicePort
	var match bool
	for _, p := range service.Spec.Ports {
		if (backend.ServicePort.Type == intstr.Int && backend.ServicePort.IntVal == p.Port) ||
			(backend.ServicePort.Type == intstr.String && backend.ServicePort.StrVal == p.Name) {
			portName = p.Name
			portSpec = p
			match = true
			break
		}
	}

	if !match {
		return nil, errors.New("service port not found")
	}

	svc := &dynamic.Service{
		LoadBalancer: &dynamic.ServersLoadBalancer{
			PassHostHeader: func(v bool) *bool { return &v }(true),
		},
	}

	svcConfig, err := parseServiceConfig(service.Annotations)
	if err != nil {
		return nil, err
	}

	if svcConfig != nil && svcConfig.Service != nil {
		svc.LoadBalancer.Sticky = svcConfig.Service.Sticky
		if svcConfig.Service.PassHostHeader != nil {
			svc.LoadBalancer.PassHostHeader = svcConfig.Service.PassHostHeader
		}
	}

	if service.Spec.Type == corev1.ServiceTypeExternalName {
		protocol := getProtocol(portSpec, portSpec.Name, svcConfig)

		svc.LoadBalancer.Servers = []dynamic.Server{
			{URL: fmt.Sprintf("%s://%s:%d", protocol, service.Spec.ExternalName, portSpec.Port)},
		}

		return svc, nil
	}

	endpoints, endpointsExists, endpointsErr := client.GetEndpoints(namespace, backend.ServiceName)
	if endpointsErr != nil {
		return nil, endpointsErr
	}

	if !endpointsExists {
		return nil, errors.New("endpoints not found")
	}

	if len(endpoints.Subsets) == 0 {
		return nil, errors.New("subset not found")
	}

	var port int32
	for _, subset := range endpoints.Subsets {
		for _, p := range subset.Ports {
			if portName == p.Name {
				port = p.Port
				break
			}
		}

		if port == 0 {
			return nil, errors.New("cannot define a port")
		}

		protocol := getProtocol(portSpec, portName, svcConfig)

		for _, addr := range subset.Addresses {
			svc.LoadBalancer.Servers = append(svc.LoadBalancer.Servers, dynamic.Server{
				URL: fmt.Sprintf("%s://%s:%d", protocol, addr.IP, port),
			})
		}
	}

	return svc, nil
}

func getProtocol(portSpec corev1.ServicePort, portName string, svcConfig *ServiceConfig) string {
	if svcConfig != nil && svcConfig.Service != nil && svcConfig.Service.ServersScheme != "" {
		return svcConfig.Service.ServersScheme
	}

	protocol := "http"
	if portSpec.Port == 443 || strings.HasPrefix(portName, "https") {
		protocol = "https"
	}

	return protocol
}

func loadRouter(rule networkingv1beta1.IngressRule, pa networkingv1beta1.HTTPIngressPath, rtConfig *RouterConfig, serviceName string) *dynamic.Router {
	var rules []string
	if len(rule.Host) > 0 {
		rules = []string{buildHostRule(rule.Host)}
	}

	if len(pa.Path) > 0 {
		matcher := defaultPathMatcher

		if pa.PathType == nil || *pa.PathType == "" || *pa.PathType == networkingv1beta1.PathTypeImplementationSpecific {
			if rtConfig != nil && rtConfig.Router != nil && rtConfig.Router.PathMatcher != "" {
				matcher = rtConfig.Router.PathMatcher
			}
		} else if *pa.PathType == networkingv1beta1.PathTypeExact {
			matcher = "Path"
		}

		rules = append(rules, fmt.Sprintf("%s(`%s`)", matcher, pa.Path))
	}

	rt := &dynamic.Router{
		Rule:    strings.Join(rules, " && "),
		Service: serviceName,
	}

	if rtConfig != nil && rtConfig.Router != nil {
		rt.Priority = rtConfig.Router.Priority
		rt.EntryPoints = rtConfig.Router.EntryPoints
		rt.Middlewares = rtConfig.Router.Middlewares

		if rtConfig.Router.TLS != nil {
			rt.TLS = rtConfig.Router.TLS
		}
	}

	return rt
}

func throttleEvents(ctx context.Context, throttleDuration time.Duration, pool *safe.Pool, eventsChan <-chan interface{}) chan interface{} {
	if throttleDuration == 0 {
		return nil
	}

	// Create a buffered channel to hold the pending event (if we're delaying processing the event due to throttling).
	eventsChanBuffered := make(chan interface{}, 1)

	// Run a goroutine that reads events from eventChan and does a
	// non-blocking write to pendingEvent. This guarantees that writing to
	// eventChan will never block, and that pendingEvent will have
	// something in it if there's been an event since we read from that channel.
	pool.GoCtx(func(ctxPool context.Context) {
		for {
			select {
			case <-ctxPool.Done():
				return
			case nextEvent := <-eventsChan:
				select {
				case eventsChanBuffered <- nextEvent:
				default:
					// We already have an event in eventsChanBuffered, so we'll
					// do a refresh as soon as our throttle allows us to. It's fine
					// to drop the event and keep whatever's in the buffer -- we
					// don't do different things for different events.
					log.FromContext(ctx).Debugf("Dropping event kind %T due to throttling", nextEvent)
				}
			}
		}
	})

	return eventsChanBuffered
}
