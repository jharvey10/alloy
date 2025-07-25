package remotecfg

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/go-kit/log"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
	"github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1/collectorv1connect"
	"github.com/grafana/alloy/internal/alloyseed"
	"github.com/grafana/alloy/internal/build"
	"github.com/grafana/alloy/internal/component/common/config"
	"github.com/grafana/alloy/internal/featuregate"
	alloy_runtime "github.com/grafana/alloy/internal/runtime"
	"github.com/grafana/alloy/internal/runtime/logging/level"
	"github.com/grafana/alloy/internal/service"
	"github.com/grafana/alloy/internal/useragent"
	"github.com/grafana/alloy/internal/util/jitter"
	"github.com/grafana/alloy/syntax"
	"github.com/grafana/alloy/syntax/ast"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	commonconfig "github.com/prometheus/common/config"
)

func getHash(in []byte) string {
	fnvHash := fnv.New32()
	fnvHash.Write(in)
	return fmt.Sprintf("%x", fnvHash.Sum(nil))
}

const baseJitter = 100 * time.Millisecond

// This value is used when we want to disable polling. We use a value that is
// slightly less than MaxInt to avoid overflowing
const disablePollingFrequency = math.MaxInt64 - baseJitter

var errNotModified = errors.New("config not modified since last fetch")

var userAgent = useragent.Get()

// Service implements a service for remote configuration.
// The default value of ch is nil; this means it will block forever if the
// remotecfg service is not configured. In addition, we're keeping track of
// the ticker so we can avoid leaking goroutines.
// The datapath field is where the service looks for the local cache location.
// It is defined as a hash of the Arguments field.
type Service struct {
	opts Options
	args Arguments

	ctrl service.Controller

	mut                  sync.RWMutex
	asClient             collectorv1connect.CollectorServiceClient
	clientFactory        func(args Arguments) (collectorv1connect.CollectorServiceClient, error)
	ticker               *jitter.Ticker
	updateTickerChan     chan struct{}
	pollFrequency        time.Duration
	dataPath             string
	lastLoadedConfigHash string
	systemAttrs          map[string]string
	attrs                map[string]string
	metrics              *metrics

	// This is the hash received from the API. It is used to determine if
	// the configuration has changed since the last fetch
	remoteHash string

	// This is the AST file parsed from the configuration. This is used
	// for the support bundle
	astFile *ast.File
}

type metrics struct {
	lastLoadSuccess      prometheus.Gauge
	lastFetchNotModified prometheus.Gauge
	totalFailures        prometheus.Counter
	configHash           *prometheus.GaugeVec
	lastFetchSuccessTime prometheus.Gauge
	totalAttempts        prometheus.Counter
	getConfigTime        prometheus.Histogram
}

// ServiceName defines the name used for the remotecfg service.
const ServiceName = "remotecfg"

const reservedAttributeNamespace = "collector"
const namespaceDelimiter = "."

// Options are used to configure the remotecfg service. Options are
// constant for the lifetime of the remotecfg service.
type Options struct {
	Logger      log.Logger            // Where to send logs.
	StoragePath string                // Where to cache configuration on-disk.
	ConfigPath  string                // Where the root config file is.
	Metrics     prometheus.Registerer // Where to send metrics to.
}

// Arguments holds runtime settings for the remotecfg service.
type Arguments struct {
	URL              string                   `alloy:"url,attr,optional"`
	ID               string                   `alloy:"id,attr,optional"`
	Name             string                   `alloy:"name,attr,optional"`
	Attributes       map[string]string        `alloy:"attributes,attr,optional"`
	PollFrequency    time.Duration            `alloy:"poll_frequency,attr,optional"`
	HTTPClientConfig *config.HTTPClientConfig `alloy:",squash"`
}

// GetDefaultArguments populates the default values for the Arguments struct.
func GetDefaultArguments() Arguments {
	return Arguments{
		ID:               alloyseed.Get().UID,
		Attributes:       make(map[string]string),
		PollFrequency:    1 * time.Minute,
		HTTPClientConfig: config.CloneDefaultHTTPClientConfig(),
	}
}

// SetToDefault implements syntax.Defaulter.
func (a *Arguments) SetToDefault() {
	*a = GetDefaultArguments()
}

// Validate implements syntax.Validator.
func (a *Arguments) Validate() error {
	if a.PollFrequency < 10*time.Second {
		return fmt.Errorf("poll_frequency must be at least \"10s\", got %q", a.PollFrequency)
	}

	for k := range a.Attributes {
		if strings.HasPrefix(k, reservedAttributeNamespace+namespaceDelimiter) {
			return fmt.Errorf("%q is a reserved namespace for remotecfg attribute keys", reservedAttributeNamespace)
		}
	}

	// We must explicitly Validate because HTTPClientConfig is squashed and it
	// won't run otherwise
	if a.HTTPClientConfig != nil {
		return a.HTTPClientConfig.Validate()
	}

	return nil
}

// Hash marshals the Arguments and returns a hash representation.
func (a *Arguments) Hash() (string, error) {
	b, err := syntax.Marshal(a)
	if err != nil {
		return "", fmt.Errorf("failed to marshal arguments: %w", err)
	}
	return getHash(b), nil
}

// New returns a new instance of the remotecfg service.
func New(opts Options) (*Service, error) {
	basePath := filepath.Join(opts.StoragePath, ServiceName)
	err := os.MkdirAll(basePath, 0750)
	if err != nil {
		return nil, err
	}

	return &Service{
		opts:             opts,
		systemAttrs:      getSystemAttributes(),
		updateTickerChan: make(chan struct{}, 1),
		pollFrequency:    disablePollingFrequency,
		clientFactory: func(args Arguments) (collectorv1connect.CollectorServiceClient, error) {
			httpClient, err := commonconfig.NewClientFromConfig(*args.HTTPClientConfig.Convert(), "remoteconfig")
			if err != nil {
				return nil, err
			}
			return collectorv1connect.NewCollectorServiceClient(
				httpClient,
				args.URL,
				connect.WithHTTPGet(),
				connect.WithInterceptors(&agentInterceptor{userAgent}),
			), nil
		},
	}, nil
}

func getSystemAttributes() map[string]string {
	return map[string]string{
		reservedAttributeNamespace + namespaceDelimiter + "version": build.Version,
		reservedAttributeNamespace + namespaceDelimiter + "os":      runtime.GOOS,
	}
}

func (s *Service) registerMetrics() {
	prom := promauto.With(s.opts.Metrics)
	mets := &metrics{
		configHash: prom.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "remotecfg_hash",
				Help: "Hash of the currently active remote configuration.",
			},
			[]string{"hash"},
		),
		lastLoadSuccess: prom.NewGauge(
			prometheus.GaugeOpts{
				Name: "remotecfg_last_load_successful",
				Help: "Remote config loaded successfully",
			},
		),
		lastFetchNotModified: prom.NewGauge(
			prometheus.GaugeOpts{
				Name: "remotecfg_last_load_not_modified",
				Help: "Remote config not modified since last fetch",
			},
		),
		totalFailures: prom.NewCounter(
			prometheus.CounterOpts{
				Name: "remotecfg_load_failures_total",
				Help: "Remote configuration load failures",
			},
		),
		totalAttempts: prom.NewCounter(
			prometheus.CounterOpts{
				Name: "remotecfg_load_attempts_total",
				Help: "Attempts to load remote configuration",
			},
		),
		lastFetchSuccessTime: prom.NewGauge(
			prometheus.GaugeOpts{
				Name: "remotecfg_last_load_success_timestamp_seconds",
				Help: "Timestamp of the last successful remote configuration load",
			},
		),
		getConfigTime: prom.NewHistogram(
			prometheus.HistogramOpts{
				Name: "remotecfg_request_duration_seconds",
				Help: "Duration of remote configuration requests.",
			},
		),
	}
	s.metrics = mets
}

// Data returns an instance of [Data]. Calls to Data are cachable by the
// caller.
// Data must only be called after Run.
func (s *Service) Data() any {
	// While the contract specifies that Data must be called after Run,
	// the other services start in parallel and Cluster attempts to access
	// the controller via Data, this locking prevents a race.
	s.mut.RLock()
	defer s.mut.RUnlock()

	if s.ctrl == nil {
		return Data{Host: nil}
	}
	host := s.ctrl.(alloy_runtime.ServiceController).GetHost()
	return Data{Host: host}
}

// Data includes information associated with the HTTP service.
type Data struct {
	// Host exposes the Host of the isolated controller that is created by the
	// remotecfg service.
	Host service.Host
}

// Definition returns the definition of the remotecfg service.
func (s *Service) Definition() service.Definition {
	return service.Definition{
		Name:       ServiceName,
		ConfigType: Arguments{},
		DependsOn:  nil, // remotecfg has no dependencies.
		Stability:  featuregate.StabilityGenerallyAvailable,
	}
}

var _ service.Service = (*Service)(nil)

// Run implements [service.Service] and starts the remotecfg service. It will
// run until the provided context is canceled or there is a fatal error.
func (s *Service) Run(ctx context.Context, host service.Host) error {
	// We need to use the mtx here as the cluster service will
	// start in parallel and attempt to access the controller via Data()
	s.mut.Lock()
	s.ctrl = host.NewController(ServiceName)
	s.mut.Unlock()

	s.fetch()
	err := s.registerCollector()
	if err != nil && err != errNoopClient {
		return err
	}

	s.ticker = jitter.NewTicker(s.pollFrequency, baseJitter)

	defer func() {
		s.ticker.Stop()
		s.ticker = nil
	}()

	// Run the service's own controller.
	go func() {
		s.ctrl.Run(ctx)
	}()

	for {
		select {
		case <-s.ticker.C:
			err := s.fetchRemote()
			if err != nil && err != errNoopClient {
				level.Error(s.opts.Logger).Log("msg", "failed to fetch remote configuration from the API", "err", err)
			}
		case <-s.updateTickerChan:
			s.ticker.Reset(s.pollFrequency)
		case <-ctx.Done():
			return nil
		}
	}
}

// Update implements [service.Service] and applies settings.
func (s *Service) Update(newConfig any) error {
	newArgs := newConfig.(Arguments)
	s.mut.Lock()

	// We either never set the block on the first place, or recently removed
	// it. Make sure we stop everything gracefully before returning.
	if newArgs.URL == "" {
		s.setPollFrequency(disablePollingFrequency)
		s.asClient = noopClient{}
		s.args.HTTPClientConfig = config.CloneDefaultHTTPClientConfig()
		s.mut.Unlock()

		s.setLastLoadedCfgHash("")
		return nil
	}

	hash, err := newArgs.Hash()
	if err != nil {
		s.mut.Unlock()
		return err
	}
	s.dataPath = filepath.Join(s.opts.StoragePath, ServiceName, hash)

	s.setPollFrequency(newArgs.PollFrequency)
	// Update the HTTP client last since it might fail.
	if !reflect.DeepEqual(s.args.HTTPClientConfig, newArgs.HTTPClientConfig) {
		client, err := s.clientFactory(newArgs)
		if err != nil {
			s.mut.Unlock()
			return err
		}
		s.asClient = client
	}
	// Combine the new attributes on top of the system attributes
	s.attrs = maps.Clone(s.systemAttrs)
	maps.Copy(s.attrs, newArgs.Attributes)

	// Update the args as the last step to avoid polluting any comparisons
	s.args = newArgs
	err = s.registerCollector()
	if err != nil {
		s.mut.Unlock()
		return err
	}

	if s.metrics == nil {
		s.registerMetrics()
	}
	s.mut.Unlock()

	// If we've already called Run, then immediately trigger an API call with
	// the updated Arguments, and/or fall back to the updated cache location.
	if s.ctrl != nil && s.ctrl.Ready() {
		s.fetch()
	}

	return nil
}

// GetCachedAstFile returns the AST file that was parsed from the configuration.
func (s *Service) GetCachedAstFile() *ast.File {
	s.mut.RLock()
	defer s.mut.RUnlock()
	return s.astFile
}

// fetch attempts to read configuration from the API and the local cache
// and then parse/load their contents in order of preference.
func (s *Service) fetch() {
	if err := s.fetchRemote(); err != nil {
		level.Error(s.opts.Logger).Log("msg", "failed to fetch remote config", "err", err)
		s.fetchLocal()
	}
}

func (s *Service) registerCollector() error {
	req := connect.NewRequest(&collectorv1.RegisterCollectorRequest{
		Id:              s.args.ID,
		LocalAttributes: s.attrs,
		Name:            s.args.Name,
	})
	client := s.asClient

	_, err := client.RegisterCollector(context.Background(), req)
	if err != nil {
		return err
	}
	return nil
}

func (s *Service) fetchRemote() error {
	if !s.isEnabled() {
		return nil
	}

	level.Debug(s.opts.Logger).Log("msg", "fetching remote configuration")

	b, err := s.getAPIConfig()
	s.metrics.totalAttempts.Add(1)

	if err == nil {
		s.metrics.lastLoadSuccess.Set(1)
		s.metrics.lastFetchSuccessTime.SetToCurrentTime()
	} else if err != errNotModified {
		s.metrics.totalFailures.Add(1)
		s.metrics.lastLoadSuccess.Set(0)
		return err
	}

	if err == errNotModified {
		level.Debug(s.opts.Logger).Log("msg", "skipping over API response since it has not been modified since last fetch")
		s.metrics.lastFetchNotModified.Set(1)
		return nil
	} else {
		s.metrics.lastFetchNotModified.Set(0)
	}

	// API returned the same configuration as the last one we loaded, no need to reload.
	newConfigHash := getHash(b)
	if s.getLastLoadedCfgHash() == newConfigHash {
		level.Debug(s.opts.Logger).Log("msg", "skipping over API response since it matched the last loaded one")
		return nil
	}

	err = s.parseAndLoad(b)
	if err != nil {
		return err
	}

	// If successful, flush to disk and keep a copy.
	s.setCachedConfig(b)
	return nil
}

func (s *Service) fetchLocal() {
	b, err := s.getCachedConfig()
	if err != nil {
		level.Error(s.opts.Logger).Log("msg", "failed to read from cache", "err", err)
		return
	}

	err = s.parseAndLoad(b)
	if err != nil {
		level.Error(s.opts.Logger).Log("msg", "failed to load from cache", "err", err)
	}
}

func (s *Service) getAPIConfig() ([]byte, error) {
	s.mut.RLock()
	req := connect.NewRequest(&collectorv1.GetConfigRequest{
		Id:              s.args.ID,
		LocalAttributes: s.attrs,
		Hash:            s.remoteHash,
	})
	client := s.asClient
	s.mut.RUnlock()

	start := time.Now()
	gcr, err := client.GetConfig(context.Background(), req)
	if err != nil {
		return nil, err
	}
	s.metrics.getConfigTime.Observe(time.Since(start).Seconds())
	if gcr.Msg.NotModified {
		return nil, errNotModified
	}
	if gcr.Msg.Hash != "" {
		s.mut.Lock()
		s.remoteHash = gcr.Msg.Hash
		s.mut.Unlock()
	}
	return []byte(gcr.Msg.GetContent()), nil
}

func (s *Service) getCachedConfig() ([]byte, error) {
	s.mut.RLock()
	p := s.dataPath
	s.mut.RUnlock()

	return os.ReadFile(p)
}

func (s *Service) setCachedConfig(b []byte) {
	s.mut.RLock()
	p := s.dataPath
	s.mut.RUnlock()

	err := os.WriteFile(p, b, 0750)
	if err != nil {
		level.Error(s.opts.Logger).Log("msg", "failed to flush remote configuration contents the on-disk cache", "err", err)
	}
}

func (s *Service) parseAndLoad(b []byte) error {
	s.mut.RLock()
	ctrl := s.ctrl
	s.mut.RUnlock()

	if len(b) == 0 {
		return nil
	}
	s.setLastLoadedCfgHash(getHash(b))
	file, err := ctrl.LoadSource(b, nil, s.opts.ConfigPath)
	if err != nil {
		return err
	}

	s.setAstFile(file)
	return nil
}

func (s *Service) getLastLoadedCfgHash() string {
	s.mut.RLock()
	defer s.mut.RUnlock()

	return s.lastLoadedConfigHash
}

func (s *Service) setAstFile(f *ast.File) {
	s.mut.Lock()
	defer s.mut.Unlock()
	s.astFile = f
}

func (s *Service) setLastLoadedCfgHash(h string) {
	s.mut.Lock()
	defer s.mut.Unlock()
	if s.metrics != nil {
		s.metrics.configHash.Reset()
		s.metrics.configHash.WithLabelValues(h).Set(1)
	}
	s.lastLoadedConfigHash = h
}

func (s *Service) isEnabled() bool {
	s.mut.RLock()
	defer s.mut.RUnlock()
	return s.args.URL != "" && s.asClient != nil
}

func (s *Service) setPollFrequency(t time.Duration) {
	s.pollFrequency = t
	select {
	// If the channel is full it means there's already an update triggered
	// or Run is not running. In both cases, we don't need to trigger another
	// update or block.
	case s.updateTickerChan <- struct{}{}:
	default:
	}
}

type agentInterceptor struct {
	agent string
}

func (i *agentInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("User-Agent", i.agent)
		return next(ctx, req)
	}
}

func (i *agentInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("User-Agent", i.agent)
		return conn
	}
}

func (i *agentInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func GetHost(host service.Host) (service.Host, error) {
	svc, found := host.GetService(ServiceName)
	if !found {
		return nil, fmt.Errorf("remote config service not available")
	}

	data := svc.Data().(Data)
	if data.Host == nil {
		return nil, fmt.Errorf("remote config service startup in progress")
	}
	return data.Host, nil
}
