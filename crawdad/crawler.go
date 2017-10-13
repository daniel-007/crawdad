package crawdad

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/schollz/pluck/pluck"
	pb "gopkg.in/cheggaaa/pb.v1"

	"golang.org/x/net/proxy"

	humanize "github.com/dustin/go-humanize"
	"github.com/go-redis/redis"
	"github.com/goware/urlx"
	"github.com/jcelliott/lumber"
	"github.com/pkg/errors"
	"github.com/schollz/collectlinks"
)

// Settings is the configuration across all instances
type Settings struct {
	BaseURL              string
	PluckConfig          string
	KeywordsToExclude    []string
	KeywordsToInclude    []string
	AllowQueryParameters bool
	AllowHashParameters  bool
	DontFollowLinks      bool
}

// Crawler is the crawler instance
type Crawler struct {
	// Instance options
	RedisURL                 string
	RedisPort                string
	MaxNumberConnections     int
	MaxNumberWorkers         int
	MaximumNumberOfErrors    int
	TimeIntervalToPrintStats int
	Debug                    bool
	Info                     bool
	UseProxy                 bool
	UserAgent                string
	EraseDB                  bool

	// Public  options
	Settings Settings

	// Private instance parameters
	log                *lumber.ConsoleLogger
	programTime        time.Time
	numberOfURLSParsed int
	numTrash           int64
	numDone            int64
	numToDo            int64
	numDoing           int64
	isRunning          bool
	errors             int64
	client             *http.Client
	todo               *redis.Client
	doing              *redis.Client
	done               *redis.Client
	trash              *redis.Client
	wg                 sync.WaitGroup
}

// New creates a new crawler instance
func New() (*Crawler, error) {
	var err error
	err = nil
	c := new(Crawler)
	c.MaxNumberConnections = 20
	c.MaxNumberWorkers = 8
	c.RedisURL = "localhost"
	c.RedisPort = "6379"
	c.TimeIntervalToPrintStats = 1
	c.MaximumNumberOfErrors = 20
	c.errors = 0
	return c, err
}

// Init initializes the connection pool and the Redis client
func (c *Crawler) Init(config ...Settings) (err error) {
	c.Logging()
	// connect to Redis for the settings
	remoteSettings := redis.NewClient(&redis.Options{
		Addr:     c.RedisURL + ":" + c.RedisPort,
		Password: "",
		DB:       4,
	})
	_, err = remoteSettings.Ping().Result()
	if err != nil {
		return errors.New(fmt.Sprintf("Redis not available at %s:%s, did you run it? The easiest way is\n\n\tdocker run -d -v `pwd`:/data -p 6379:6379 redis\n\n", c.RedisURL, c.RedisPort))
	}
	if len(config) > 0 {
		// save the supplied configuration to Redis
		bSettings, err := json.Marshal(config[0])
		_, err = remoteSettings.Set("settings", string(bSettings), 0).Result()
		if err != nil {
			return err
		}
		c.log.Info("saved settings: %v", config[0])
	}
	// load the configuration from Redis
	var val string
	val, err = remoteSettings.Get("settings").Result()
	if err != nil {
		return errors.New(fmt.Sprintf("You need to set the base settings. Use\n\n\tcrawdad -s %s -p %s -set -url http://www.URL.com\n\n", c.RedisURL, c.RedisPort))
	}
	err = json.Unmarshal([]byte(val), &c.Settings)
	c.log.Info("loaded settings: %v", c.Settings)

	// Generate the connection pool
	var tr *http.Transport
	if c.UseProxy {
		tbProxyURL, err := url.Parse("socks5://127.0.0.1:9050")
		if err != nil {
			c.log.Fatal("Failed to parse proxy URL: %v\n", err)
			return err
		}
		tbDialer, err := proxy.FromURL(tbProxyURL, proxy.Direct)
		if err != nil {
			c.log.Fatal("Failed to obtain proxy dialer: %v\n", err)
			return err
		}
		tr = &http.Transport{
			MaxIdleConns:       c.MaxNumberConnections,
			IdleConnTimeout:    15 * time.Second,
			DisableCompression: true,
			Dial:               tbDialer.Dial,
		}
	} else {
		tr = &http.Transport{
			MaxIdleConns:       c.MaxNumberConnections,
			IdleConnTimeout:    15 * time.Second,
			DisableCompression: true,
		}
	}
	c.client = &http.Client{
		Transport: tr,
		Timeout:   time.Duration(10 * time.Second),
	}

	// Setup Redis client
	c.todo = redis.NewClient(&redis.Options{
		Addr:        c.RedisURL + ":" + c.RedisPort,
		Password:    "", // no password set
		DB:          0,  // use default DB
		ReadTimeout: 30 * time.Second,
		MaxRetries:  10,
	})
	c.doing = redis.NewClient(&redis.Options{
		Addr:        c.RedisURL + ":" + c.RedisPort,
		Password:    "", // no password set
		DB:          1,  // use default DB
		ReadTimeout: 30 * time.Second,
		MaxRetries:  10,
	})
	c.done = redis.NewClient(&redis.Options{
		Addr:        c.RedisURL + ":" + c.RedisPort,
		Password:    "", // no password set
		DB:          2,  // use default DB
		ReadTimeout: 30 * time.Second,
		MaxRetries:  10,
	})
	c.trash = redis.NewClient(&redis.Options{
		Addr:        c.RedisURL + ":" + c.RedisPort,
		Password:    "", // no password set
		DB:          3,  // use default DB
		ReadTimeout: 30 * time.Second,
		MaxRetries:  10,
	})

	if c.EraseDB {
		c.log.Info("Flushed database")
		err = c.Flush()
		if err != nil {
			return err
		}
	}
	if len(c.Settings.BaseURL) > 0 {
		c.log.Info("Adding %s to URLs", c.Settings.BaseURL)
		err = c.AddSeeds([]string{c.Settings.BaseURL})
		if err != nil {
			return err
		}
	}
	return
}

func (c *Crawler) Logging() {
	// Generate the logging
	if c.Info {
		c.log = lumber.NewConsoleLogger(lumber.INFO)
	} else if c.Debug {
		c.log = lumber.NewConsoleLogger(lumber.TRACE)
	} else {
		c.log = lumber.NewConsoleLogger(lumber.WARN)
	}
}

func (c *Crawler) Redo() (err error) {
	var keys []string
	keys, err = c.doing.Keys("*").Result()
	if err != nil {
		return
	}
	for _, key := range keys {
		c.log.Trace("Moving %s back to todo list", key)
		_, err = c.doing.Del(key).Result()
		if err != nil {
			c.log.Error(err.Error())
		}
		_, err = c.todo.Set(key, "", 0).Result()
		if err != nil {
			c.log.Error(err.Error())
		}
	}

	keys, err = c.trash.Keys("*").Result()
	if err != nil {
		return
	}
	for _, key := range keys {
		c.log.Trace("Moving %s back to todo list", key)
		_, err = c.trash.Del(key).Result()
		if err != nil {
			c.log.Error(err.Error())
		}
		_, err = c.todo.Set(key, "", 0).Result()
		if err != nil {
			c.log.Error(err.Error())
		}
	}

	return
}

func (c *Crawler) DumpMap() (m map[string]string, err error) {
	fmt.Println("Dumping...")
	totalSize := int64(0)
	var tempSize int64
	tempSize, _ = c.done.DbSize().Result()
	totalSize = tempSize * 2
	bar := pb.StartNew(int(totalSize))
	defer bar.Finish()

	var keySize int64
	var keys []string
	keySize, _ = c.done.DbSize().Result()
	keys = make([]string, keySize+10000)
	i := 0
	iter := c.done.Scan(0, "", 0).Iterator()
	for iter.Next() {
		bar.Increment()
		keys[i] = iter.Val()
		i++
	}
	keys = keys[:i]
	if err = iter.Err(); err != nil {
		c.log.Error("Problem getting done")
		return
	}
	m = make(map[string]string)
	for _, key := range keys {
		bar.Increment()
		var val string
		val, err = c.done.Get(key).Result()
		if err != nil {
			return
		}
		m[key] = val
	}
	return
}

func (c *Crawler) Dump() (allKeys []string, err error) {
	fmt.Println("Dumping...")
	allKeys = make([]string, 0)
	var keySize int64
	var keys []string

	totalSize := int64(0)
	var tempSize int64
	tempSize, _ = c.todo.DbSize().Result()
	totalSize += tempSize
	tempSize, _ = c.done.DbSize().Result()
	totalSize += tempSize
	tempSize, _ = c.doing.DbSize().Result()
	totalSize += tempSize
	tempSize, _ = c.trash.DbSize().Result()
	totalSize += tempSize
	bar := pb.StartNew(int(totalSize))
	defer bar.Finish()

	keySize, _ = c.todo.DbSize().Result()
	keys = make([]string, keySize)
	i := 0
	iter := c.todo.Scan(0, "", 0).Iterator()
	for iter.Next() {
		bar.Increment()
		keys[i] = iter.Val()
		i++
	}
	if err := iter.Err(); err != nil {
		c.log.Error("Problem getting todo")
		return nil, err
	}
	allKeys = append(allKeys, keys...)

	keySize, _ = c.doing.DbSize().Result()
	keys = make([]string, keySize)
	i = 0
	iter = c.doing.Scan(0, "", 0).Iterator()
	for iter.Next() {
		bar.Increment()
		keys[i] = iter.Val()
		i++
	}
	if err := iter.Err(); err != nil {
		c.log.Error("Problem getting doing")
		return nil, err
	}
	allKeys = append(allKeys, keys...)

	keySize, _ = c.done.DbSize().Result()
	keys = make([]string, keySize)
	i = 0
	iter = c.done.Scan(0, "", 0).Iterator()
	for iter.Next() {
		bar.Increment()
		keys[i] = iter.Val()
		i++
	}
	if err := iter.Err(); err != nil {
		c.log.Error("Problem getting done")
		return nil, err
	}
	allKeys = append(allKeys, keys...)

	keySize, _ = c.trash.DbSize().Result()
	keys = make([]string, keySize)
	i = 0
	iter = c.trash.Scan(0, "", 0).Iterator()
	for iter.Next() {
		bar.Increment()
		keys[i] = iter.Val()
		i++
	}
	if err := iter.Err(); err != nil {
		c.log.Error("Problem getting trash")
		return nil, err
	}
	allKeys = append(allKeys, keys...)
	return
}

func (c *Crawler) getIP() (ip string, err error) {
	req, err := http.NewRequest("GET", "http://icanhazip.com", nil)
	if err != nil {
		c.log.Error("Problem making request")
		return
	}
	if c.UserAgent != "" {
		c.log.Trace("Setting useragent string to '%s'", c.UserAgent)
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	ipB, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	ip = string(ipB)
	return
}

func (c *Crawler) addLinkToDo(link string, force bool) (err error) {
	if !force {
		// add only if it isn't already in one of the databases
		_, err = c.todo.Get(link).Result()
		if err != redis.Nil {
			return
		}
		_, err = c.doing.Get(link).Result()
		if err != redis.Nil {
			return
		}
		_, err = c.done.Get(link).Result()
		if err != redis.Nil {
			return
		}
		_, err = c.trash.Get(link).Result()
		if err != redis.Nil {
			return
		}
	}

	// add it to the todo list
	err = c.todo.Set(link, "", 0).Err()
	return
}

// Flush erases the database
func (c *Crawler) Flush() (err error) {
	_, err = c.todo.FlushAll().Result()
	if err != nil {
		return
	}
	_, err = c.done.FlushAll().Result()
	if err != nil {
		return
	}
	_, err = c.doing.FlushAll().Result()
	if err != nil {
		return
	}
	_, err = c.trash.FlushAll().Result()
	if err != nil {
		return
	}
	return
}

func (c *Crawler) scrapeLinks(url string) (linkCandidates []string, pluckedData string, err error) {
	c.log.Trace("Scraping %s", url)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		c.log.Error("Problem making request for %s: %s", url, err.Error())
		return nil, "", nil
	}
	if c.UserAgent != "" {
		c.log.Trace("Setting useragent string to '%s'", c.UserAgent)
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.log.Error("Problem doing request for %s: %s", url, err.Error())
		return nil, "", nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		c.doing.Del(url).Result()
		c.todo.Del(url).Result()
		c.trash.Set(url, "", 0).Result()
		c.errors++
		if c.errors > int64(c.MaximumNumberOfErrors) {
			err = errors.New("too many errors!")
			return
		}
		return
	}

	// reset errors as long as the code is good
	c.errors = 0

	// copy resp.Body
	var bodyBytes []byte
	bodyBytes, _ = ioutil.ReadAll(resp.Body)
	resp.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))

	// do plucking
	if c.Settings.PluckConfig != "" {
		plucker, _ := pluck.New()
		err = plucker.LoadFromString(c.Settings.PluckConfig)
		if err != nil {
			return
		}
		err = plucker.Pluck(bufio.NewReader(bytes.NewReader(bodyBytes)))
		if err != nil {
			return
		}
		pluckedData = plucker.ResultJSON()
	}

	if c.Settings.DontFollowLinks {
		return
	}

	// collect links
	links := collectlinks.All(resp.Body)

	// find good links
	linkCandidates = make([]string, len(links))
	linkCandidatesI := 0
	for _, link := range links {
		c.log.Trace(link)
		// disallow query parameters, if not flagged
		if strings.Contains(link, "?") && !c.Settings.AllowQueryParameters {
			link = strings.Split(link, "?")[0]
		}

		// disallow hash parameters, if not flagged
		if strings.Contains(link, "#") && !c.Settings.AllowHashParameters {
			link = strings.Split(link, "#")[0]
		}

		// add Base URL if it doesn't have
		if !strings.Contains(link, "http") && len(link) > 2 {
			if c.Settings.BaseURL[len(c.Settings.BaseURL)-1] != '/' && link[0] != '/' {
				link = "/" + link
			}
			link = c.Settings.BaseURL + link
		}

		// skip links that have a different Base URL
		if !strings.Contains(link, c.Settings.BaseURL) {
			// c.log.Trace("Skipping %s because it has a different base URL", link)
			continue
		}

		// normalize the link
		parsedLink, _ := urlx.Parse(link)
		normalizedLink, _ := urlx.Normalize(parsedLink)
		if len(normalizedLink) == 0 {
			continue
		}

		// Exclude keywords, skip if any are found
		foundExcludedKeyword := false
		for _, keyword := range c.Settings.KeywordsToExclude {
			if strings.Contains(normalizedLink, keyword) {
				foundExcludedKeyword = true
				// c.log.Trace("Skipping %s because contains %s", link, keyword)
				break
			}
		}
		if foundExcludedKeyword {
			continue
		}

		// Include keywords, skip if any are NOT found
		foundIncludedKeyword := false
		for _, keyword := range c.Settings.KeywordsToInclude {
			if strings.Contains(normalizedLink, keyword) {
				foundIncludedKeyword = true
				break
			}
		}
		if !foundIncludedKeyword && len(c.Settings.KeywordsToInclude) > 0 {
			continue
		}

		// If it passed all the tests, add to link candidates
		linkCandidates[linkCandidatesI] = normalizedLink
		linkCandidatesI++
	}
	// trim candidate list
	linkCandidates = linkCandidates[0:linkCandidatesI]

	return
}

func (c *Crawler) crawl(id int, jobs <-chan string, results chan<- error) {
	for randomURL := range jobs {
		// time the link getting process
		t := time.Now()

		c.log.Trace("Got work in %s", time.Since(t).String())
		urls, pluckedData, err := c.scrapeLinks(randomURL)
		if err != nil {
			results <- err
			continue
		}

		t = time.Now()

		// move url to 'done'
		_, err = c.doing.Del(randomURL).Result()
		if err != nil {
			results <- err
			continue
		}
		_, err = c.done.Set(randomURL, pluckedData, 0).Result()
		if err != nil {
			results <- err
			continue
		}

		// add new urls to 'todo'
		for _, url := range urls {
			c.addLinkToDo(url, false)
		}
		if len(urls) > 0 {
			c.log.Info("worker #%d: %d urls from %s [%s]", id, len(urls), randomURL, time.Since(t).String())
		}
		c.numberOfURLSParsed++
		results <- nil
	}
}

func (c *Crawler) AddSeeds(seeds []string) (err error) {
	// add beginning link
	var bar *pb.ProgressBar
	if len(seeds) > 100 {
		fmt.Println("Adding seeds...")
		bar = pb.StartNew(len(seeds))
		defer bar.Finish()
	}
	for _, seed := range seeds {
		if len(seeds) > 100 {
			bar.Increment()
		}
		err = c.addLinkToDo(seed, true)
		if err != nil {
			return
		}
	}
	c.log.Info("Added %d seed links", len(seeds))
	return
}

// Crawl initiates the pool of connections and begins
// scraping URLs according to the todo list
func (c *Crawler) Crawl() (err error) {
	fmt.Printf("\nStarting crawl on %s\n\n", c.Settings.BaseURL)
	buf := new(bytes.Buffer)
	if err := toml.NewEncoder(buf).Encode(c.Settings); err != nil {
		return err
	}
	fmt.Println("Settings:")
	fmt.Println(buf.String())
	c.programTime = time.Now()
	c.numberOfURLSParsed = 0
	go c.contantlyPrintStats()
	defer c.stopCrawling()
	for {
		// check if there are any links to do
		dbsize, err := c.todo.DbSize().Result()
		if err != nil {
			return err
		}

		// break if there are no links to do
		if dbsize == 0 {
			c.log.Info("No more work to do!")
			break
		}

		urlsToDo := make([]string, c.MaxNumberWorkers)
		maxI := 0
		for i := 0; i < c.MaxNumberWorkers; i++ {
			randomURL, err := c.todo.RandomKey().Result()
			if err != nil {
				continue
			}
			urlsToDo[i] = randomURL
			maxI = i

			// place in 'doing'
			_, err = c.todo.Del(randomURL).Result()
			if err != nil {
				return errors.Wrap(err, "problem removing from todo")
			}
			_, err = c.doing.Set(randomURL, "", 0).Result()
			if err != nil {
				return errors.Wrap(err, "problem placing in doing")
			}

		}
		urlsToDo = urlsToDo[:maxI+1]

		jobs := make(chan string, len(urlsToDo))
		results := make(chan error, len(urlsToDo))

		for w := range urlsToDo {
			go c.crawl(w, jobs, results)
		}
		for _, j := range urlsToDo {
			jobs <- j
		}
		close(jobs)

		for a := 0; a < len(urlsToDo); a++ {
			err := <-results
			if err != nil {
				return err
			}
		}
	}
	return
}

func (c *Crawler) stopCrawling() {
	c.isRunning = false
	c.printStats()
}

func round(f float64) int {
	if math.Abs(f) < 0.5 {
		return 0
	}
	return int(f + math.Copysign(0.5, f))
}

func (c *Crawler) updateListCounts() (err error) {
	// Update stats
	c.numToDo, err = c.todo.DbSize().Result()
	if err != nil {
		return
	}
	c.numDoing, err = c.doing.DbSize().Result()
	if err != nil {
		return
	}
	c.numDone, err = c.done.DbSize().Result()
	if err != nil {
		return
	}
	c.numTrash, err = c.trash.DbSize().Result()
	if err != nil {
		return
	}
	return nil
}

func (c *Crawler) contantlyPrintStats() {
	c.isRunning = true
	fmt.Println(`                                           parsed speed   todo     done     doing   trash      errors
                                                (urls/min)`)
	for {
		time.Sleep(time.Duration(int32(c.TimeIntervalToPrintStats)) * time.Second)
		c.updateListCounts()
		c.printStats()
		if !c.isRunning {
			fmt.Println("Finished")
			return
		}
	}
}

func (c *Crawler) printStats() {
	URLSPerSecond := round(60.0 * float64(c.numberOfURLSParsed) / float64(time.Since(c.programTime).Seconds()))
	printURL := strings.Replace(c.Settings.BaseURL, "https://", "", 1)
	printURL = strings.Replace(printURL, "http://", "", 1)
	if len(printURL) > 17 {
		printURL = printURL[:17]
	}
	log.Printf("[%17s] %9s %3d %8s %8s %8s %8s %8s\n",
		printURL,
		humanize.Comma(int64(c.numberOfURLSParsed)),
		URLSPerSecond,
		humanize.Comma(int64(c.numToDo)),
		humanize.Comma(int64(c.numDone)),
		humanize.Comma(int64(c.numDoing)),
		humanize.Comma(int64(c.numTrash)),
		humanize.Comma(int64(c.errors)))
}
