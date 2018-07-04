package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"github.com/fnproject/fn/api/agent"
	"github.com/fnproject/fn/api/agent/hybrid"
	"github.com/fnproject/fn/api/common"
	"github.com/fnproject/fn/api/datastore"
	"github.com/fnproject/fn/api/id"
	"github.com/fnproject/fn/api/logs"
	"github.com/fnproject/fn/api/models"
	"github.com/fnproject/fn/api/mqs"
	pool "github.com/fnproject/fn/api/runnerpool"
	"github.com/fnproject/fn/api/version"
	"github.com/fnproject/fn/fnext"
	"github.com/gin-gonic/gin"
	zipkinhttp "github.com/openzipkin/zipkin-go/reporter/http"
	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"go.opencensus.io/exporter/jaeger"
	"go.opencensus.io/exporter/prometheus"
	"go.opencensus.io/exporter/zipkin"
	"go.opencensus.io/plugin/ochttp"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/trace"
)

const (
	// TODO these are kind of redundant as exported values since the env vars
	// have to be set using these values (hopefully from an env), consider
	// forcing usage through WithXxx configuration methods and documenting there vs.
	// expecting users to use os.SetEnv(EnvLogLevel, "debug") // why ?

	// EnvLogLevel sets the stderr logging level
	EnvLogLevel = "FN_LOG_LEVEL"

	// EnvLogDest is a url of a log destination:
	// possible schemes: { udp, tcp, file }
	// file url must contain only a path, syslog must contain only a host[:port]
	// expect: [scheme://][host][:port][/path]
	// default scheme to udp:// if none given
	EnvLogDest = "FN_LOG_DEST"

	// EnvLogPrefix is a prefix to affix to each log line.
	EnvLogPrefix = "FN_LOG_PREFIX"

	// EnvMQURL is a url to an MQ service:
	// possible out-of-the-box schemes: { memory, redis, bolt }
	EnvMQURL = "FN_MQ_URL"

	// EnvDBURL is a url to a db service:
	// possible schemes: { postgres, sqlite3, mysql }
	EnvDBURL = "FN_DB_URL"

	// EnvLogDBURL is a url to a log storage service:
	// possible schemes: { postgres, sqlite3, mysql, s3 }
	EnvLogDBURL = "FN_LOGSTORE_URL"

	// EnvRunnerURL is a url pointing to an Fn API service.
	EnvRunnerURL = "FN_RUNNER_API_URL"

	// EnvRunnerAddresses is a list of runner urls for an lb to use.
	EnvRunnerAddresses = "FN_RUNNER_ADDRESSES"

	// EnvPublicLoadBalancerURL is the url to inject into trigger responses to get a public url.
	EnvPublicLoadBalancerURL = "FN_PUBLIC_LB_URL"

	// EnvNodeType defines the runtime mode for fn to run in, options
	// are one of: { full, api, lb, runner, pure-runner }
	EnvNodeType = "FN_NODE_TYPE"

	// EnvPort is the port to listen on for fn http server.
	EnvPort = "FN_PORT" // be careful, Gin expects this variable to be "port"

	// EnvGRPCPort is the port to run the grpc server on for a pure-runner node.
	EnvGRPCPort = "FN_GRPC_PORT"

	// EnvAPICORSOrigins is the list of CORS origins to allow.
	EnvAPICORSOrigins = "FN_API_CORS_ORIGINS"

	// EnvAPICORSHeaders is the list of CORS headers allowed.
	EnvAPICORSHeaders = "FN_API_CORS_HEADERS"

	// EnvZipkinURL is the url of a zipkin node to send traces to.
	EnvZipkinURL = "FN_ZIPKIN_URL"

	// EnvJaegerURL is the url of a jaeger node to send traces to.
	EnvJaegerURL = "FN_JAEGER_URL"

	// EnvCert is the certificate used to communicate with other fn nodes.
	EnvCert = "FN_NODE_CERT"

	// EnvCertKey is the key for the specified cert.
	EnvCertKey = "FN_NODE_CERT_KEY"

	// EnvCertAuth is the CA for the cert provided.
	EnvCertAuth = "FN_NODE_CERT_AUTHORITY"

	// EnvRIDHeader is the header name of the incoming request which holds the request ID
	EnvRIDHeader = "FN_RID_HEADER"

	// EnvProcessCollectorList is the list of procid's to collect metrics for.
	EnvProcessCollectorList = "FN_PROCESS_COLLECTOR_LIST"

	// EnvLBPlacementAlg is the algorithm to place fn calls to fn runners in lb.[0w
	EnvLBPlacementAlg = "FN_PLACER"

	// DefaultLogLevel is info
	DefaultLogLevel = "info"

	// DefaultLogDest is stderr
	DefaultLogDest = "stderr"

	// DefaultPort is 8080
	DefaultPort = 8080

	// DefaultGRPCPort is 9190
	DefaultGRPCPort = 9190
)

// NodeType is the mode to run fn in.
type NodeType int32

const (
	// ServerTypeFull runs all API endpoints, including executing tasks.
	ServerTypeFull NodeType = iota

	// ServerTypeAPI runs only /v1 endpoints, to manage resources.
	ServerTypeAPI

	// ServerTypeLB runs only /r/ endpoints, routing to runner nodes.
	ServerTypeLB

	// ServerTypeRunner runs only /r/ endpoints, to execute tasks.
	ServerTypeRunner

	// ServerTypePureRunner runs only grpc server, to execute tasks.
	ServerTypePureRunner
)

func (s NodeType) String() string {
	switch s {
	default:
		return "full"
	case ServerTypeAPI:
		return "api"
	case ServerTypeLB:
		return "lb"
	case ServerTypeRunner:
		return "runner"
	case ServerTypePureRunner:
		return "pure-runner"
	}
}

// Server is the object which ties together all the fn things, it is the entrypoint
// for managing fn resources and executing tasks.
type Server struct {
	Router      *gin.Engine
	AdminRouter *gin.Engine

	webListenPort    int
	adminListenPort  int
	grpcListenPort   int
	agent            agent.Agent
	datastore        models.Datastore
	mq               models.MessageQueue
	logstore         models.LogStore
	nodeType         NodeType
	cert             string
	certKey          string
	certAuthority    string
	appListeners     *appListeners
	routeListeners   *routeListeners
	fnListeners      *fnListeners
	triggerListeners *triggerListeners
	rootMiddlewares  []fnext.Middleware
	apiMiddlewares   []fnext.Middleware
	promExporter     *prometheus.Exporter
	triggerAnnotator TriggerAnnotator
	// Extensions can append to this list of contexts so that cancellations are properly handled.
	extraCtxs []context.Context
}

func nodeTypeFromString(value string) NodeType {
	switch value {
	case "api":
		return ServerTypeAPI
	case "lb":
		return ServerTypeLB
	case "runner":
		return ServerTypeRunner
	case "pure-runner":
		return ServerTypePureRunner
	default:
		return ServerTypeFull
	}
}

// NewFromEnv creates a new Functions server based on env vars.
func NewFromEnv(ctx context.Context, opts ...Option) *Server {
	curDir := pwd()
	var defaultDB, defaultMQ string
	nodeType := nodeTypeFromString(getEnv(EnvNodeType, "")) // default to full
	switch nodeType {
	case ServerTypeLB: // nothing
	case ServerTypeRunner: // nothing
	case ServerTypePureRunner: // nothing
	default:
		// only want to activate these for full and api nodes
		defaultDB = fmt.Sprintf("sqlite3://%s/data/fn.db", curDir)
		defaultMQ = fmt.Sprintf("bolt://%s/data/fn.mq", curDir)
	}
	opts = append(opts, WithWebPort(getEnvInt(EnvPort, DefaultPort)))
	opts = append(opts, WithGRPCPort(getEnvInt(EnvGRPCPort, DefaultGRPCPort)))
	opts = append(opts, WithLogLevel(getEnv(EnvLogLevel, DefaultLogLevel)))
	opts = append(opts, WithLogDest(getEnv(EnvLogDest, DefaultLogDest), getEnv(EnvLogPrefix, "")))
	opts = append(opts, WithZipkin(getEnv(EnvZipkinURL, "")))
	opts = append(opts, WithJaeger(getEnv(EnvJaegerURL, "")))
	opts = append(opts, WithPrometheus()) // TODO option to turn this off?
	opts = append(opts, WithDBURL(getEnv(EnvDBURL, defaultDB)))
	opts = append(opts, WithMQURL(getEnv(EnvMQURL, defaultMQ)))
	opts = append(opts, WithLogURL(getEnv(EnvLogDBURL, "")))
	opts = append(opts, WithRunnerURL(getEnv(EnvRunnerURL, "")))
	opts = append(opts, WithType(nodeType))
	opts = append(opts, WithNodeCert(getEnv(EnvCert, "")))
	opts = append(opts, WithNodeCertKey(getEnv(EnvCertKey, "")))
	opts = append(opts, WithNodeCertAuthority(getEnv(EnvCertAuth, "")))

	publicLbUrl := getEnv(EnvPublicLoadBalancerURL, "")
	if publicLbUrl != "" {
		logrus.Infof("using LB Base URL: '%s'", publicLbUrl)
		opts = append(opts, WithTriggerAnnotator(NewStaticURLTriggerAnnotator(publicLbUrl)))
	} else {
		opts = append(opts, WithTriggerAnnotator(NewRequestBasedTriggerAnnotator()))
	}

	// Agent handling depends on node type and several other options so it must be the last processed option.
	// Also we only need to create an agent if this is not an API node.
	if nodeType != ServerTypeAPI {
		opts = append(opts, WithAgentFromEnv())
	} else {
		// NOTE: ensures logstore is set or there will be troubles
		opts = append(opts, WithLogstoreFromDatastore())
	}

	return New(ctx, opts...)
}

func pwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		logrus.WithError(err).Fatalln("couldn't get working directory, possibly unsupported platform?")
	}
	// Replace forward slashes in case this is windows, URL parser errors
	return strings.Replace(cwd, "\\", "/", -1)
}

// WithWebPort maps EnvPort
func WithWebPort(port int) Option {
	return func(ctx context.Context, s *Server) error {
		if s.adminListenPort == s.webListenPort {
			s.adminListenPort = port
		}
		s.webListenPort = port
		return nil
	}
}

// WithGRPCPort maps EnvGRPCPort
func WithGRPCPort(port int) Option {
	return func(ctx context.Context, s *Server) error {
		s.grpcListenPort = port
		return nil
	}
}

// WithLogLevel maps EnvLogLevel
func WithLogLevel(ll string) Option {
	return func(ctx context.Context, s *Server) error {
		common.SetLogLevel(ll)
		return nil
	}
}

// WithLogDest maps EnvLogDest
func WithLogDest(dst, prefix string) Option {
	return func(ctx context.Context, s *Server) error {
		common.SetLogDest(dst, prefix)
		return nil
	}
}

// WithDBURL maps EnvDBURL
func WithDBURL(dbURL string) Option {
	return func(ctx context.Context, s *Server) error {
		if dbURL != "" {
			ds, err := datastore.New(ctx, dbURL)
			if err != nil {
				return err
			}
			s.datastore = ds
		}
		return nil
	}
}

// WithMQURL maps EnvMQURL
func WithMQURL(mqURL string) Option {
	return func(ctx context.Context, s *Server) error {
		if mqURL != "" {
			mq, err := mqs.New(mqURL)
			if err != nil {
				return err
			}
			s.mq = mq
		}
		return nil
	}
}

// WithLogURL maps EnvLogURL
func WithLogURL(logstoreURL string) Option {
	return func(ctx context.Context, s *Server) error {
		if ldb := logstoreURL; ldb != "" {
			logDB, err := logs.New(ctx, logstoreURL)
			if err != nil {
				return err
			}
			s.logstore = logDB
		}
		return nil
	}
}

// WithRunnerURL maps EnvRunnerURL
func WithRunnerURL(runnerURL string) Option {
	return func(ctx context.Context, s *Server) error {
		if runnerURL != "" {
			cl, err := hybrid.NewClient(runnerURL)
			if err != nil {
				return err
			}
			s.agent = agent.New(agent.NewCachedDataAccess(cl))
		}
		return nil
	}
}

// WithType maps EnvNodeType
func WithType(t NodeType) Option {
	return func(ctx context.Context, s *Server) error {
		s.nodeType = t
		return nil
	}
}

// WithNodeCert maps EnvNodeCert
func WithNodeCert(cert string) Option {
	return func(ctx context.Context, s *Server) error {
		if cert != "" {
			abscert, err := filepath.Abs(cert)
			if err != nil {
				return fmt.Errorf("Unable to resolve %v: please specify a valid and readable cert file", cert)
			}
			_, err = os.Stat(abscert)
			if err != nil {
				return fmt.Errorf("Cannot stat %v: please specify a valid and readable cert file", abscert)
			}
			s.cert = abscert
		}
		return nil
	}
}

// WithNodeCertKey maps EnvNodeCertKey
func WithNodeCertKey(key string) Option {
	return func(ctx context.Context, s *Server) error {
		if key != "" {
			abskey, err := filepath.Abs(key)
			if err != nil {
				return fmt.Errorf("Unable to resolve %v: please specify a valid and readable cert key file", key)
			}
			_, err = os.Stat(abskey)
			if err != nil {
				return fmt.Errorf("Cannot stat %v: please specify a valid and readable cert key file", abskey)
			}
			s.certKey = abskey
		}
		return nil
	}
}

// WithNodeCertAuthority maps EnvNodeCertAuthority
func WithNodeCertAuthority(ca string) Option {
	return func(ctx context.Context, s *Server) error {
		if ca != "" {
			absca, err := filepath.Abs(ca)
			if err != nil {
				return fmt.Errorf("Unable to resolve %v: please specify a valid and readable cert authority file", ca)
			}
			_, err = os.Stat(absca)
			if err != nil {
				return fmt.Errorf("Cannot stat %v: please specify a valid and readable cert authority file", absca)
			}
			s.certAuthority = absca
		}
		return nil
	}
}

// WithDatastore allows directly setting a datastore
func WithDatastore(ds models.Datastore) Option {
	return func(ctx context.Context, s *Server) error {
		s.datastore = ds
		return nil
	}
}

// WithMQ allows directly setting an MQ
func WithMQ(mq models.MessageQueue) Option {
	return func(ctx context.Context, s *Server) error {
		s.mq = mq
		return nil
	}
}

// WithLogstore allows directly setting a logstore
func WithLogstore(ls models.LogStore) Option {
	return func(ctx context.Context, s *Server) error {
		s.logstore = ls
		return nil
	}
}

// WithAgent allows directly setting an agent
func WithAgent(agent agent.Agent) Option {
	return func(ctx context.Context, s *Server) error {
		s.agent = agent
		return nil
	}
}

func (s *Server) defaultRunnerPool() (pool.RunnerPool, error) {
	runnerAddresses := getEnv(EnvRunnerAddresses, "")
	if runnerAddresses == "" {
		return nil, errors.New("must provide FN_RUNNER_ADDRESSES  when running in default load-balanced mode")
	}
	return agent.DefaultStaticRunnerPool(strings.Split(runnerAddresses, ",")), nil
}

// WithLogstoreFromDatastore sets the logstore to the datastore, iff
// the datastore implements the logstore interface.
func WithLogstoreFromDatastore() Option {
	return func(ctx context.Context, s *Server) error {
		if s.datastore == nil {
			return errors.New("Need a datastore in order to use it as a logstore")
		}
		if s.logstore == nil {
			if ls, ok := s.datastore.(models.LogStore); ok {
				s.logstore = ls
			} else {
				return errors.New("datastore must implement logstore interface")
			}
		}
		return nil
	}
}

// WithFullAgent is a shorthand for WithAgent(... create a full agent here ...)
func WithFullAgent() Option {
	return func(ctx context.Context, s *Server) error {
		s.nodeType = ServerTypeFull

		// ensure logstore is set (TODO compat only?)
		if s.logstore == nil {
			WithLogstoreFromDatastore()(ctx, s)
		}

		if s.datastore == nil || s.logstore == nil || s.mq == nil {
			return errors.New("full nodes must configure FN_DB_URL, FN_LOG_URL, FN_MQ_URL")
		}
		s.agent = agent.New(agent.NewCachedDataAccess(agent.NewDirectDataAccess(s.datastore, s.logstore, s.mq)))
		return nil
	}
}

// WithAgentFromEnv must be provided as the last server option because it relies
// on all other options being set first.
func WithAgentFromEnv() Option {
	return func(ctx context.Context, s *Server) error {
		switch s.nodeType {
		case ServerTypeAPI:
			return errors.New("should not initialize an agent for an Fn API node")
		case ServerTypeRunner:
			runnerURL := getEnv(EnvRunnerURL, "")
			if runnerURL == "" {
				return errors.New("no FN_RUNNER_API_URL provided for an Fn Runner node")
			}
			cl, err := hybrid.NewClient(runnerURL)
			if err != nil {
				return err
			}
			s.agent = agent.New(agent.NewCachedDataAccess(cl))
		case ServerTypePureRunner:
			if s.datastore != nil {
				return errors.New("pure runner nodes must not be configured with a datastore (FN_DB_URL)")
			}
			if s.mq != nil {
				return errors.New("pure runner nodes must not be configured with a message queue (FN_MQ_URL)")
			}
			ds, err := hybrid.NewNopDataStore()
			if err != nil {
				return err
			}
			grpcAddr := fmt.Sprintf(":%d", s.grpcListenPort)
			cancelCtx, cancel := context.WithCancel(ctx)
			prAgent, err := agent.DefaultPureRunner(cancel, grpcAddr, ds, s.cert, s.certKey, s.certAuthority)
			if err != nil {
				return err
			}
			s.agent = prAgent
			s.extraCtxs = append(s.extraCtxs, cancelCtx)
		case ServerTypeLB:
			s.nodeType = ServerTypeLB
			runnerURL := getEnv(EnvRunnerURL, "")
			if runnerURL == "" {
				return errors.New("no FN_RUNNER_API_URL provided for an Fn NuLB node")
			}
			if s.datastore != nil {
				return errors.New("lb nodes must not be configured with a datastore (FN_DB_URL)")
			}
			if s.mq != nil {
				return errors.New("lb nodes must not be configured with a message queue (FN_MQ_URL)")
			}

			cl, err := hybrid.NewClient(runnerURL)
			if err != nil {
				return err
			}

			runnerPool, err := s.defaultRunnerPool()
			if err != nil {
				return err
			}

			// Select the placement algorithm
			var placer pool.Placer
			switch getEnv(EnvLBPlacementAlg, "") {
			case "ch":
				placer = pool.NewCHPlacer()
			default:
				placer = pool.NewNaivePlacer()
			}

			keys := []string{"fn_appname", "fn_path"}
			pool.RegisterPlacerViews(keys)
			agent.RegisterLBAgentViews(keys)

			s.agent, err = agent.NewLBAgent(agent.NewCachedDataAccess(cl), runnerPool, placer)
			if err != nil {
				return errors.New("LBAgent creation failed")
			}
		default:
			WithFullAgent()(ctx, s)
		}
		return nil
	}
}

// WithExtraCtx appends a context to the list of contexts the server will watch for cancellations / errors / signals.
func WithExtraCtx(extraCtx context.Context) Option {
	return func(ctx context.Context, s *Server) error {
		s.extraCtxs = append(s.extraCtxs, extraCtx)
		return nil
	}
}

//WithTriggerAnnotator adds a trigggerEndpoint provider to the server
func WithTriggerAnnotator(provider TriggerAnnotator) Option {
	return func(ctx context.Context, s *Server) error {
		s.triggerAnnotator = provider
		return nil
	}
}

// WithAdminServer starts the admin server on the specified port.
func WithAdminServer(port int) Option {
	return func(ctx context.Context, s *Server) error {
		s.AdminRouter = gin.New()
		s.adminListenPort = port
		return nil
	}
}

// New creates a new Functions server with the opts given. For convenience, users may
// prefer to use NewFromEnv but New is more flexible if needed.
func New(ctx context.Context, opts ...Option) *Server {
	ctx, span := trace.StartSpan(ctx, "server_init")
	defer span.End()

	log := common.Logger(ctx)
	engine := gin.New()
	s := &Server{
		Router:      engine,
		AdminRouter: engine,
		// Add default ports
		webListenPort:   DefaultPort,
		adminListenPort: DefaultPort,
		grpcListenPort:  DefaultGRPCPort,
		// Almost everything else is configured through opts (see NewFromEnv for ex.) or below
	}

	for _, opt := range opts {
		if opt == nil {
			continue
		}
		err := opt(ctx, s)
		if err != nil {
			log.WithError(err).Fatal("Error during server opt initialization.")
		}
	}

	// Check that WithAgent options have been processed correctly.
	switch s.nodeType {
	case ServerTypeAPI:
		if s.agent != nil {
			log.Fatal("Incorrect configuration, API nodes must not have an agent initialized.")
		}
		if s.triggerAnnotator == nil {
			log.Fatal("No trigger annotatator  set ")
		}
	default:
		if s.agent == nil {
			log.Fatal("Incorrect configuration, non-API nodes must have an agent initialized.")
		}
	}

	setMachineID()
	s.Router.Use(loggerWrap, traceWrap, panicWrap) // TODO should be opts
	optionalCorsWrap(s.Router)                     // TODO should be an opt
	apiMetricsWrap(s)
	s.bindHandlers(ctx)

	s.appListeners = new(appListeners)
	s.routeListeners = new(routeListeners)
	s.fnListeners = new(fnListeners)
	s.triggerListeners = new(triggerListeners)

	s.datastore = datastore.Wrap(s.datastore)
	s.datastore = fnext.NewDatastore(s.datastore, s.appListeners, s.routeListeners, s.fnListeners, s.triggerListeners)
	s.logstore = logs.Wrap(s.logstore)

	return s
}

// WithPrometheus activates the prometheus collection and /metrics endpoint
func WithPrometheus() Option {
	return func(ctx context.Context, s *Server) error {
		reg := promclient.NewRegistry()
		reg.MustRegister(promclient.NewProcessCollector(os.Getpid(), "fn"),
			promclient.NewGoCollector(),
		)

		for _, exeName := range getMonitoredCmdNames() {
			san := promSanitizeMetricName(exeName)
			err := reg.Register(promclient.NewProcessCollectorPIDFn(getPidCmd(exeName), san))
			if err != nil {
				panic(err)
			}
		}

		exporter, err := prometheus.NewExporter(prometheus.Options{
			Namespace: "fn",
			Registry:  reg,
			OnError:   func(err error) { logrus.WithError(err).Error("opencensus prometheus exporter err") },
		})
		if err != nil {
			return fmt.Errorf("error starting prometheus exporter: %v", err)
		}
		s.promExporter = exporter
		view.RegisterExporter(exporter)
		registerViews()
		return nil
	}
}

// WithJaeger maps EnvJaegerURL
func WithJaeger(jaegerURL string) Option {
	return func(ctx context.Context, s *Server) error {
		// ex: "http://localhost:14268"
		if jaegerURL == "" {
			return nil
		}

		exporter, err := jaeger.NewExporter(jaeger.Options{
			Endpoint:    jaegerURL,
			ServiceName: "fn",
		})
		if err != nil {
			return fmt.Errorf("error connecting to jaeger: %v", err)
		}
		trace.RegisterExporter(exporter)
		logrus.WithFields(logrus.Fields{"url": jaegerURL}).Info("exporting spans to jaeger")

		// TODO don't do this. testing parity.
		trace.ApplyConfig(trace.Config{DefaultSampler: trace.AlwaysSample()})
		return nil
	}
}

// WithZipkin maps EnvZipkinURL
func WithZipkin(zipkinURL string) Option {
	return func(ctx context.Context, s *Server) error {
		// ex: "http://zipkin:9411/api/v2/spans"

		if zipkinURL == "" {
			return nil
		}

		reporter := zipkinhttp.NewReporter(zipkinURL, zipkinhttp.MaxBacklog(10000))
		exporter := zipkin.NewExporter(reporter, nil)
		trace.RegisterExporter(exporter)
		logrus.WithFields(logrus.Fields{"url": zipkinURL}).Info("exporting spans to zipkin")

		// TODO don't do this. testing parity.
		trace.ApplyConfig(trace.Config{DefaultSampler: trace.AlwaysSample()})
		return nil
	}
}

// prometheus only allows [a-zA-Z0-9:_] in metrics names.
func promSanitizeMetricName(name string) string {
	res := make([]rune, 0, len(name))
	for _, rVal := range name {
		if unicode.IsDigit(rVal) || unicode.IsLetter(rVal) || rVal == ':' {
			res = append(res, rVal)
		} else {
			res = append(res, '_')
		}
	}
	return string(res)
}

// determine sidecar-monitored cmd names. But by default
// we track dockerd + containerd
func getMonitoredCmdNames() []string {

	// override? empty variable to disable trackers
	val, ok := os.LookupEnv(EnvProcessCollectorList)
	if ok {
		return strings.Fields(val)
	}

	// by default, we monitor dockerd and containerd
	return []string{"dockerd", "docker-containerd"}
}

// TODO plumbing considerations, we've put the S pipe next to the chandalier...
func getPidCmd(cmdName string) func() (int, error) {
	// prometheus' process collector only works on linux anyway. let them do the
	// process detection, if we return an error here we just get 0 metrics and it
	// does not log / blow up (that's fine!) it's also likely we hit permissions
	// errors here for many installations, we want to do similar and ignore (we
	// just want for prod).

	var pid int

	return func() (int, error) {
		if pid != 0 {
			// make sure it's our pid.
			if isPidMatchCmd(cmdName, pid) {
				return pid, nil
			}
			pid = 0 // reset to go search
		}

		if pids, err := getPidList(); err == nil {
			for _, test := range pids {
				if isPidMatchCmd(cmdName, test) {
					pid = test
					return pid, nil
				}
			}
		}

		return pid, io.EOF
	}
}

func isPidMatchCmd(cmdName string, pid int) bool {
	fs, err := os.Open("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	defer fs.Close()

	rd := bufio.NewReader(fs)
	tok, err := rd.ReadSlice(0)
	if err != nil || len(tok) < len(cmdName) {
		return false
	}

	return filepath.Base(string(tok[:len(tok)-1])) == cmdName
}

func getPidList() ([]int, error) {
	var pids []int
	dir, err := os.Open("/proc")
	if err != nil {
		return pids, nil
	}
	defer dir.Close()

	files, err := dir.Readdirnames(0)
	if err != nil {
		return pids, nil
	}

	pids = make([]int, 0, len(files))
	for _, tok := range files {
		if conv, err := strconv.ParseUint(tok, 10, 64); err == nil {
			pids = append(pids, int(conv))
		}
	}
	return pids, nil
}

func setMachineID() {
	port := uint16(getEnvInt(EnvPort, DefaultPort))
	addr := whoAmI().To4()
	if addr == nil {
		addr = net.ParseIP("127.0.0.1").To4()
		logrus.Warn("could not find non-local ipv4 address to use, using '127.0.0.1' for ids, if this is a cluster beware of duplicate ids!")
	}
	id.SetMachineIdHost(addr, port)
}

// whoAmI searches for a non-local address on any network interface, returning
// the first one it finds. it could be expanded to search eth0 or en0 only but
// to date this has been unnecessary.
func whoAmI() net.IP {
	ints, _ := net.Interfaces()
	for _, i := range ints {
		if i.Name == "docker0" || i.Name == "lo" {
			// not perfect
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			ip, _, err := net.ParseCIDR(a.String())
			if a.Network() == "ip+net" && err == nil && ip.To4() != nil {
				if !bytes.Equal(ip, net.ParseIP("127.0.0.1")) {
					return ip
				}
			}
		}
	}
	return nil
}

func extractFields(c *gin.Context) logrus.Fields {
	fields := logrus.Fields{"action": path.Base(c.HandlerName())}
	for _, param := range c.Params {
		fields[param.Key] = param.Value
	}
	return fields
}

// Start runs any configured machinery, including the http server, agent, etc.
// Start will block until the context is cancelled or times out.
func (s *Server) Start(ctx context.Context) {
	newctx, cancel := contextWithSignal(ctx, os.Interrupt, syscall.SIGTERM)
	s.startGears(newctx, cancel)
}

func (s *Server) startGears(ctx context.Context, cancel context.CancelFunc) {
	// By default it serves on :8080 unless a
	// FN_PORT environment variable was defined.
	listen := fmt.Sprintf(":%d", s.webListenPort)

	const runHeader = `
        ______
       / ____/___
      / /_  / __ \
     / __/ / / / /
    /_/   /_/ /_/`
	fmt.Println(runHeader)
	fmt.Printf("        v%s\n\n", version.Version)

	logrus.WithField("type", s.nodeType).Infof("Fn serving on `%v`", listen)

	installChildReaper()

	server := http.Server{
		Addr:    listen,
		Handler: &ochttp.Handler{Handler: s.Router},

		// TODO we should set read/write timeouts
	}

	go func() {
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			logrus.WithError(err).Error("server error")
			cancel()
		} else {
			logrus.Info("server stopped")
		}
	}()

	if s.webListenPort != s.adminListenPort {
		adminListen := fmt.Sprintf(":%d", s.adminListenPort)
		logrus.WithField("type", s.nodeType).Infof("Fn Admin serving on `%v`", adminListen)
		adminServer := http.Server{
			Addr:    adminListen,
			Handler: &ochttp.Handler{Handler: s.AdminRouter},
		}

		go func() {
			err := adminServer.ListenAndServe()
			if err != nil && err != http.ErrServerClosed {
				logrus.WithError(err).Error("server error")
				cancel()
			} else {
				logrus.Info("server stopped")
			}
		}()

		defer func() {
			if err := adminServer.Shutdown(context.Background()); err != nil {
				logrus.WithError(err).Error("admin server shutdown error")
			}
		}()
	}

	// listening for signals or listener errors or cancellations on all registered contexts.
	s.extraCtxs = append(s.extraCtxs, ctx)
	cases := make([]reflect.SelectCase, len(s.extraCtxs))
	for i, ctx := range s.extraCtxs {
		cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())}
	}
	nth, recv, wasSend := reflect.Select(cases)
	if wasSend {
		logrus.WithFields(logrus.Fields{
			"ctxNumber":     nth,
			"receivedValue": recv.String(),
		}).Debug("Stopping because of received value from done context.")
	} else {
		logrus.WithFields(logrus.Fields{
			"ctxNumber": nth,
		}).Debug("Stopping because of closed channel from done context.")
	}

	// TODO: do not wait forever during graceful shutdown (add graceful shutdown timeout)
	if err := server.Shutdown(context.Background()); err != nil {
		logrus.WithError(err).Error("server shutdown error")
	}

	if s.agent != nil {
		err := s.agent.Close() // after we stop taking requests, wait for all tasks to finish
		if err != nil {
			logrus.WithError(err).Error("Fail to close the agent")
		}
	}
}

func (s *Server) bindHandlers(ctx context.Context) {
	engine := s.Router
	admin := s.AdminRouter
	// now for extensible middleware
	engine.Use(s.rootMiddlewareWrapper())

	engine.GET("/", handlePing)
	admin.GET("/version", handleVersion)

	// TODO: move under v1 ?
	if s.promExporter != nil {
		admin.GET("/metrics", gin.WrapH(s.promExporter))
	}

	profilerSetup(admin, "/debug")

	// Pure runners don't have any route, they have grpc
	if s.nodeType != ServerTypePureRunner {
		if s.nodeType != ServerTypeRunner {
			clean := engine.Group("/v1")
			v1 := clean.Group("")
			v1.Use(setAppNameInCtx)
			v1.Use(s.apiMiddlewareWrapper())
			v1.GET("/apps", s.handleV1AppList)
			v1.POST("/apps", s.handleV1AppCreate)

			{
				apps := v1.Group("/apps/:appName")
				apps.Use(appNameCheck)

				{
					withAppCheck := apps.Group("")
					withAppCheck.Use(s.checkAppPresenceByName())

					withAppCheck.GET("", s.handleV1AppGetByName)
					withAppCheck.PATCH("", s.handleV1AppUpdate)
					withAppCheck.DELETE("", s.handleV1AppDelete)

					withAppCheck.GET("/routes", s.handleRouteList)
					withAppCheck.GET("/routes/:route", s.handleRouteGetAPI)
					withAppCheck.PATCH("/routes/*route", s.handleRoutesPatch)
					withAppCheck.DELETE("/routes/*route", s.handleRouteDelete)
					withAppCheck.GET("/calls/:call", s.handleCallGet)
					withAppCheck.GET("/calls/:call/log", s.handleCallLogGet)
					withAppCheck.GET("/calls", s.handleCallList)
				}

				apps.POST("/routes", s.handleRoutesPostPut)
				apps.PUT("/routes/*route", s.handleRoutesPostPut)
			}

			cleanv2 := engine.Group("/v2")
			v2 := cleanv2.Group("")
			v2.Use(s.apiMiddlewareWrapper())

			{
				v2.GET("/apps", s.handleAppList)
				v2.POST("/apps", s.handleAppCreate)
				v2.GET("/apps/:appID", s.handleAppGet)
				v2.PUT("/apps/:appID", s.handleAppUpdate)
				v2.DELETE("/apps/:appID", s.handleAppDelete)

				v2.GET("/fns", s.handleFnList)
				v2.POST("/fns", s.handleFnCreate)
				v2.GET("/fns/:fnID", s.handleFnGet)
				v2.PUT("/fns/:fnID", s.handleFnUpdate)
				v2.DELETE("/fns/:fnID", s.handleFnDelete)

				v2.GET("/triggers", s.handleTriggerList)
				v2.POST("/triggers", s.handleTriggerCreate)
				v2.GET("/triggers/:triggerID", s.handleTriggerGet)
				v2.PUT("/triggers/:triggerID", s.handleTriggerUpdate)
				v2.DELETE("/triggers/:triggerID", s.handleTriggerDelete)
			}

			{
				runner := clean.Group("/runner")
				runner.PUT("/async", s.handleRunnerEnqueue)
				runner.GET("/async", s.handleRunnerDequeue)

				runner.POST("/start", s.handleRunnerStart)
				runner.POST("/finish", s.handleRunnerFinish)

				runnerAppApi := runner.Group(

					"/apps/:appID")
				runnerAppApi.Use(setAppIDInCtx)
				runnerAppApi.GET("", s.handleV1AppGetByName)
				runnerAppApi.GET("/routes/:route", s.handleRouteGetRunner)

			}
		}

		if s.nodeType != ServerTypeAPI {
			runner := engine.Group("/r")
			runner.Use(s.checkAppPresenceByNameAtRunner())
			runner.Any("/:appName", s.handleFunctionCall)
			runner.Any("/:appName/*route", s.handleFunctionCall)
		}

	}

	engine.NoRoute(func(c *gin.Context) {
		var err error
		switch {
		case s.nodeType == ServerTypeAPI && strings.HasPrefix(c.Request.URL.Path, "/r/"):
			err = models.ErrInvokeNotSupported
		case s.nodeType == ServerTypeRunner && strings.HasPrefix(c.Request.URL.Path, "/v1/"):
			err = models.ErrAPINotSupported
		default:
			var e models.APIError = models.ErrPathNotFound
			err = models.NewAPIError(e.Code(), fmt.Errorf("%v: %s", e.Error(), c.Request.URL.Path))
		}
		handleV1ErrorResponse(c, err)
	})

}

// Datastore implements fnext.ExtServer
func (s *Server) Datastore() models.Datastore {
	return s.datastore
}

// Agent implements fnext.ExtServer
func (s *Server) Agent() agent.Agent {
	return s.agent
}

// returns the unescaped ?cursor and ?perPage values
// pageParams clamps 0 < ?perPage <= 100 and defaults to 30 if 0
// ignores parsing errors and falls back to defaults.
func pageParams(c *gin.Context, base64d bool) (cursor string, perPage int) {
	cursor = c.Query("cursor")
	if base64d {
		cbytes, _ := base64.RawURLEncoding.DecodeString(cursor)
		cursor = string(cbytes)
	}

	perPage, _ = strconv.Atoi(c.Query("per_page"))
	if perPage > 100 {
		perPage = 100
	} else if perPage <= 0 {
		perPage = 30
	}
	return cursor, perPage
}

func pageParamsV2(c *gin.Context) (cursor string, perPage int) {
	cursor = c.Query("cursor")

	perPage, _ = strconv.Atoi(c.Query("per_page"))
	if perPage > 100 {
		perPage = 100
	} else if perPage <= 0 {
		perPage = 30
	}
	return cursor, perPage
}

type appResponse struct {
	Message string      `json:"message"`
	App     *models.App `json:"app"`
}

//TODO deprecate with V1
type appsV1Response struct {
	Message    string        `json:"message"`
	NextCursor string        `json:"next_cursor"`
	Apps       []*models.App `json:"apps"`
}

type routeResponse struct {
	Message string        `json:"message"`
	Route   *models.Route `json:"route"`
}

type routesResponse struct {
	Message    string          `json:"message"`
	NextCursor string          `json:"next_cursor"`
	Routes     []*models.Route `json:"routes"`
}

type callResponse struct {
	Message string       `json:"message"`
	Call    *models.Call `json:"call"`
}

type callsResponse struct {
	Message    string         `json:"message"`
	NextCursor string         `json:"next_cursor"`
	Calls      []*models.Call `json:"calls"`
}
