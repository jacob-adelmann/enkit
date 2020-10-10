package kconfig

import (
	"fmt"
	"github.com/enfabrica/enkit/lib/cache"
	"github.com/enfabrica/enkit/lib/config/marshal"
	"github.com/enfabrica/enkit/lib/config/remote"
	"github.com/enfabrica/enkit/lib/kflags"
	"github.com/enfabrica/enkit/lib/khttp/downloader"
	"github.com/enfabrica/enkit/lib/khttp/kcache"
	"github.com/enfabrica/enkit/lib/khttp/protocol"
	"github.com/enfabrica/enkit/lib/khttp/workpool"
	"github.com/enfabrica/enkit/lib/logger"
	"github.com/enfabrica/enkit/lib/multierror"
	"github.com/enfabrica/enkit/lib/retry"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

type resolver struct {
	cond     *sync.Cond
	err      error
	instance kflags.Augmenter
}

type ConfigAugmenter struct {
	// Operations on individual resolvers must be done under lock.
	lock     sync.RWMutex
	resolver []resolver
}

// Parse unmarshals a blob of bytes retrieved from a file or URL into a Config object.
func Parse(name string, data []byte) (*Config, error) {
	var config Config
	err := marshal.UnmarshalDefault(name, data, marshal.Json, &config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

// SeenStack is an object used to prevent inclusion and redirect loops.
//
// It is meant to keep track of all previously seen URLs, the nesting level,
// and return an error if any of the URLs is encountered again.
//
// SeenStack is thread safe: once created, all its methods can safely be invoked
// from any thread.
type SeenStack struct {
	lock sync.RWMutex
	seen []string
}

// NewSeenStack creates a new SeenStack.
func NewSeenStack() *SeenStack {
	return &SeenStack{}
}

// Add adds a new URL to the SeenStack.
//
// Always returns the nesting levels, how many URLs were seen already.
//
// If the url added is known, and was already visited, the function also
// returns a fmt.Errorf() with a helpful message to help debug the problem.
func (bl *SeenStack) Add(url string) (int, error) {
	bl.lock.Lock()
	defer bl.lock.Unlock()
	for _, el := range bl.seen {
		if el == url {
			return len(bl.seen), fmt.Errorf("including %s will cause a loop - full inclusion stack:\n  %s", url, strings.Join(bl.seen, "\n  "))
		}
	}
	bl.seen = append(bl.seen, url)
	return len(bl.seen), nil
}

// Stack returns the list of URLs already visited.
func (bl *SeenStack) Stack() []string {
	bl.lock.Lock()
	defer bl.lock.Unlock()
	return append([]string{}, bl.seen...)
}

type options struct {
	// Downloader to use. Either one is supplied WithDownloader, or one will be created.
	dl *downloader.Downloader
	// Options set on the downloader if a downloader is created by this library.
	dlo downloader.Modifiers
	// Options to set on each Get request, regardless of the downloader supplied.
	// This is convenient to use to share a downloader with the rest of the program.
	getOptions downloader.Modifiers

	// Options set when performing TXT DNS queries to discover the endpoint to fetch the configuration from.
	// Those DNS options do not affect normal resolution of DNS queries.
	dnso remote.DNSModifiers

	// Our beloved logging framework.
	log logger.Logger

	creator Factory
	base    string

	blocklist      *SeenStack
	recursionLimit int
}

func DefaultOptions() *options {
	return &options{
		log:            logger.Nil,
		recursionLimit: 10,
	}
}

type Modifier func(*options) error

func WithDNSOptions(mods ...remote.DNSModifier) Modifier {
	return func(o *options) error {
		o.dnso = append(o.dnso, mods...)
		return nil
	}
}

func WithGetOptions(mods ...downloader.Modifier) Modifier {
	return func(o *options) error {
		o.getOptions = append(o.getOptions, mods...)
		return nil
	}
}

func WithDownloaderOptions(mods ...downloader.Modifier) Modifier {
	return func(o *options) error {
		o.dlo = append(o.dlo, mods...)
		return nil
	}
}

func WithOptions(tocopy *options) Modifier {
	return func(o *options) error {
		*o = *tocopy
		return nil
	}
}

func WithRecursionLimit(recursionLimit int) Modifier {
	return func(o *options) error {
		o.recursionLimit = recursionLimit
		return nil
	}
}

func WithSeenStack(bl *SeenStack) Modifier {
	return func(o *options) error {
		o.blocklist = bl
		return nil
	}
}

func WithDownloader(dl *downloader.Downloader) Modifier {
	return func(o *options) error {
		o.dl = dl
		return nil
	}
}

func WithCreator(c Factory) Modifier {
	return func(o *options) error {
		o.creator = c
		return nil
	}
}

func WithBaseURL(url string) Modifier {
	return func(o *options) error {
		o.base = url
		return nil
	}
}

func WithLogger(l logger.Logger) Modifier {
	return func(o *options) error {
		o.log = l
		return nil
	}
}

type Modifiers []Modifier

func (mods Modifiers) Apply(o *options) {
	for _, m := range mods {
		m(o)
	}
}

type Flags struct {
	Downloader     *downloader.Flags
	DNS            *remote.DNSFlags
	RecursionLimit int
}

func DefaultFlags() *Flags {
	options := DefaultOptions()
	flags := &Flags{
		Downloader:     downloader.DefaultFlags(),
		DNS:            remote.DefaultDNSFlags(),
		RecursionLimit: options.recursionLimit,
	}

	// The size of the queue in the workpool used by the resolver is key in preventing deadlocks.
	// Let's make it large enough so a deadlock is extremely unlikely.
	flags.Downloader.Workpool.QueueSize = 1024
	flags.Downloader.Workpool.ImmediateQueueSize = 16
	return flags
}

func (fl *Flags) Register(set kflags.FlagSet, prefix string) *Flags {
	options := DefaultOptions()

	fl.Downloader.Register(set, prefix+"kflags-")
	fl.DNS.Register(set, prefix+"kflags-")

	set.IntVar(&fl.RecursionLimit, prefix+"kflags-recursion-limit", options.recursionLimit, "How many nested includes to process at most")
	return fl
}

func FromFlags(fl *Flags) Modifier {
	return func(o *options) error {
		if fl == nil {
			return nil
		}
		o.dlo = append(o.dlo, downloader.FromFlags(fl.Downloader))
		o.dnso = append(o.dnso, remote.FromDNSFlags(fl.DNS))
		o.recursionLimit = fl.RecursionLimit
		return nil
	}
}

func NewConfigAugmenterFromDNS(cs cache.Store, domain string, binary string, mods ...Modifier) (*ConfigAugmenter, error) {
	options := DefaultOptions()
	Modifiers(mods).Apply(options)

	if domain == "" {
		return nil, fmt.Errorf("cannot look up empty domain name")
	}

	dns := remote.NewDNS(domain, append([]remote.DNSModifier{remote.WithLogger(options.log)}, options.dnso...)...)
	eps, errs := dns.GetEndpoints()
	if len(eps) <= 0 {
		return nil, multierror.NewOr(errs, fmt.Errorf("no endpoints for domain '%s' could be detected - configure TXT records for %s? No connectivity?", domain, dns.Name()))
	}

	type Options struct {
		Timeout  time.Duration
		Fuzzy    time.Duration
		Wait     time.Duration
		Attempts int
	}

	addoptions := func(mod downloader.Modifier) {
		options.getOptions = append(options.getOptions, mod)
	}
	if options.dl == nil {
		addoptions = func(mod downloader.Modifier) {
			options.dlo = append(options.getOptions, mod)
		}
	}

	errs = []error{}
	for ix, ep := range eps {
		dnsoptions := &Options{
			Timeout:  3 * time.Second,
			Wait:     1 * time.Second,
			Fuzzy:    1 * time.Second,
			Attempts: 3,
		}

		unknown, err := ep.Options.Apply(dnsoptions)
		if err != nil {
			options.log.Warnf("Could not apply options by %s for %s: %s", domain, ep.URL.String(), err)
			continue
		}
		if len(unknown) > 0 {
			options.log.Warnf("DNS query for %s returned unknown options for %s: %s", domain, ep.URL.String(), strings.Join(unknown, ", "))
		}

		if dnsoptions.Timeout > 0 {
			addoptions(downloader.WithProtocolOptions(protocol.WithTimeout(dnsoptions.Timeout)))
		}
		if dnsoptions.Attempts > 0 || dnsoptions.Fuzzy > 0 || dnsoptions.Wait > 0 {
			ropts := []retry.Modifier{retry.WithDescription(fmt.Sprintf("config endpoint %d", ix))}
			if dnsoptions.Attempts > 0 {
				ropts = append(ropts, retry.WithAttempts(dnsoptions.Attempts))
			}
			if dnsoptions.Fuzzy > 0 {
				ropts = append(ropts, retry.WithFuzzy(dnsoptions.Fuzzy))
			}
			if dnsoptions.Wait > 0 {
				ropts = append(ropts, retry.WithWait(dnsoptions.Wait))
			}
			addoptions(downloader.WithRetryOptions(ropts...))
		}
		ep.URL.Path = path.Join(ep.URL.Path, binary+".config")
		resolver, err := NewConfigAugmenterFromURL(cs, ep.URL.String(), WithOptions(options))
		if err == nil {
			return resolver, nil
		}
		errs = append(errs, err)
	}
	return nil, multierror.NewOr(errs, fmt.Errorf("No suitable endpoint detected from record %s", dns.Name()))
}

func NewConfigAugmenterFromURL(cs cache.Store, url string, mods ...Modifier) (*ConfigAugmenter, error) {
	return NewConfigAugmenter(cs, &Config{Include: []string{url}}, mods...)
}

func NewConfigAugmenter(cs cache.Store, config *Config, mods ...Modifier) (*ConfigAugmenter, error) {
	options := DefaultOptions()
	Modifiers(mods).Apply(options)

	if options.blocklist == nil {
		options.blocklist = NewSeenStack()
	}
	var err error
	if options.dl == nil {
		options.dl, err = downloader.New(options.dlo...)
		if err != nil {
			return nil, err
		}
	}
	if options.creator == nil {
		options.creator = NewCreator(options.log, cs, options.dl, options.getOptions...).Create
	}

	namespace, err := NewNamespaceAugmenter(config.Namespace, options.creator)
	if err != nil {
		return nil, err
	}

	cr := &ConfigAugmenter{
		resolver: make([]resolver, len(config.Include)+1),
	}

	baseURL := &url.URL{}
	if options.base != "" {
		baseURL, err = url.Parse(options.base)
		if err != nil {
			return nil, fmt.Errorf("invalid base url %s - %w", options.base, err)
		}
	}

	// When visiting flags, the last include takes priority.
	//
	// The code here does two important things:
	// - fetches the includes in reverse order - from the last to the first.
	//   This is because if a flag is found in the last include, there is no
	//   reason to parse the previous one, as later definitions override previous.
	// - saves the resolvers in priority order - from most important (last) to least important (first).
	//   This simplifies the visiting: just go one resolver after the next.
	var errs []error
	cr.resolver[0] = resolver{instance: namespace, cond: sync.NewCond(&cr.lock)}
	for ix := len(config.Include) - 1; ix >= 0; ix-- {
		offset := len(config.Include) - ix

		includeURL, err := url.Parse(config.Include[ix])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		url := baseURL.ResolveReference(includeURL).String()

		size, err := options.blocklist.Add(url)
		if err != nil {
			options.log.Warnf("when loading defaults via URL, a loop was detected and stopped. Stack: %v", options.blocklist.Stack())
			continue
		}
		if options.recursionLimit > 0 && size >= options.recursionLimit {
			options.log.Warnf("when loading defaults via URL, we exceeded the recursion limit of %d. Stack: %v", options.recursionLimit, options.blocklist.Stack())
			break
		}

		cr.resolver[offset].cond = sync.NewCond(&cr.lock)
		options.dl.Get(url, protocol.Read(protocol.Callback(func(data []byte) error {
			config, err := Parse(url, data)

			// TODO: we could easily implement an error type that causes WithCache (if used) to retry with the stale data.
			if err != nil {
				return fmt.Errorf("couldn't parse %s - %w", url, err)
			}

			// TODO: this call can cause a deadlock.
			//
			// This callback won't complete until all downloads have been queued, but running
			// this function is blocking a worker. If the queue fills up, this will block forever.
			ncr, err := NewConfigAugmenter(cs, config, WithOptions(options), WithBaseURL(url))
			if err != nil {
				return fmt.Errorf("config not accepted - %w", err)
			}

			cr.lock.Lock()
			defer cr.lock.Unlock()
			if cr.resolver == nil {
				return fmt.Errorf("terminated before loading")
			}
			cr.resolver[offset].instance = ncr
			cr.resolver[offset].cond.Signal()
			return nil
		})), workpool.ErrorCallback(func(err error) {
			cr.lock.Lock()
			defer cr.lock.Unlock()

			if cr.resolver == nil {
				return
			}
			cr.resolver[offset].err = err
			cr.resolver[offset].cond.Signal()
		}), append([]downloader.Modifier{downloader.WithProtocolOptions(kcache.WithCache(cs))}, options.getOptions...)...)
	}
	return cr, multierror.New(errs)
}

func (cr *ConfigAugmenter) Visit(ns string, flag kflags.Flag) (bool, error) {
	for ix := range cr.resolver {
		resolver, err := cr.getAugmenter(ix)
		if err != nil {
			continue
		}

		found, err := resolver.Visit(ns, flag)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

func (cr *ConfigAugmenter) getAugmenter(ix int) (kflags.Augmenter, error) {
	cr.lock.RLock()
	instance, err := cr.resolver[ix].instance, cr.resolver[ix].err
	cr.lock.RUnlock()
	if instance != nil || err != nil {
		return instance, err
	}

	cr.lock.Lock()
	defer cr.lock.Unlock()
	for cr.resolver[ix].instance == nil && cr.resolver[ix].err == nil {
		cr.resolver[ix].cond.Wait()
	}
	return cr.resolver[ix].instance, cr.resolver[ix].err
}

func (cr *ConfigAugmenter) Done() error {
	cr.lock.Lock()
	list := cr.resolver
	cr.resolver = nil
	cr.lock.Unlock()

	var errs []error
	for _, res := range list {
		if res.err != nil {
			errs = append(errs, res.err)
		}
		if res.instance == nil {
			continue
		}
		if err := res.instance.Done(); err != nil {
			errs = append(errs, err)
		}
	}
	return multierror.New(errs)
}
