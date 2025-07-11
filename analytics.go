package analytics

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Version of the client.
const Version = "4.2.1"

// This interface is the main API exposed by the analytics package.
// Values that satsify this interface are returned by the client constructors
// provided by the package and provide a way to send messages via the HTTP API.
type Client interface {
	io.Closer

	// Queues a message to be sent by the client when the conditions for a batch
	// upload are met.
	// This is the main method you'll be using, a typical flow would look like
	// this:
	//
	//	client := analytics.New(writeKey)
	//	...
	//	client.Enqueue(analytics.Track{ ... })
	//	...
	//	client.Close()
	//
	// The method returns an error if the message queue not be queued, which
	// happens if the client was already closed at the time the method was
	// called or if the message was malformed.
	Enqueue(Message) error
}

type client struct {
	Config
	key string

	// This channel is where the `Enqueue` method writes messages so they can be
	// picked up and pushed by the backend goroutine taking care of applying the
	// batching rules.
	msgs chan Message

	// These two channels are used to synchronize the client shutting down when
	// `Close` is called.
	// The first channel is closed to signal the backend goroutine that it has
	// to stop, then the second one is closed by the backend goroutine to signal
	// that it has finished flushing all queued messages.
	quit       chan struct{}
	shutdown   chan struct{}
	totalNodes int
	// This HTTP client is used to send requests to the backend, it uses the
	// HTTP transport provided in the configuration.
	http http.Client
}

type batchRequest struct {
	UserID      string `json:"userId"`
	AnonymousID string `json:"anonymousId"`
}

type clusterInfoResponse struct {
	NodeCount int `json:"nodeCount"`
}

// Instantiate a new client that uses the write key passed as first argument to
// send messages to the backend.
// The client is created with the default configuration.
func New(writeKey string, dataPlaneUrl string) Client {
	// Here we can ignore the error because the default config is always valid.
	config := Config{
		DataPlaneUrl: dataPlaneUrl,
	}
	c, _ := NewWithConfig(writeKey, config)
	return c
}

// Instantiate a new client that uses the write key and configuration passed as
// arguments to send messages to the backend.
// The function will return an error if the configuration contained impossible
// values (like a negative flush interval for example).
// When the function returns an error the returned client will always be nil.
func NewWithConfig(writeKey string, config Config) (cli Client, err error) {
	if err = config.validate(); err != nil {
		return
	}

	c := &client{
		Config:   makeConfig(config),
		key:      writeKey,
		msgs:     make(chan Message, 100),
		quit:     make(chan struct{}),
		shutdown: make(chan struct{}),
		http:     makeHttpClient(config.Transport),
	}
	c.totalNodes = 1

	go c.loop()

	cli = c
	return
}

func makeHttpClient(transport http.RoundTripper) http.Client {
	httpClient := http.Client{
		Transport: transport,
	}
	if supportsTimeout(transport) {
		httpClient.Timeout = 10 * time.Second
	}
	return httpClient
}

func makeContext(context *Context) *Context {
	if context == nil {
		context = &Context{}
	}
	// context := Context{}
	context.Library = LibraryInfo{
		Name:    "analytics-go",
		Version: Version,
	}

	return context
}

func makeAnonymousId(anonymousId string) string {
	if anonymousId == "" {
		return uid()
	} else {
		return anonymousId
	}
}

func dereferenceMessage(msg Message) Message {
	switch m := msg.(type) {
	case *Alias:
		if m == nil {
			return nil
		}

		return *m
	case *Group:
		if m == nil {
			return nil
		}

		return *m
	case *Identify:
		if m == nil {
			return nil
		}

		return *m
	case *Page:
		if m == nil {
			return nil
		}

		return *m
	case *Screen:
		if m == nil {
			return nil
		}

		return *m
	case *Track:
		if m == nil {
			return nil
		}

		return *m
	}

	return msg
}

func (c *client) Enqueue(msg Message) (err error) {
	msg = dereferenceMessage(msg)
	if err = msg.Validate(); err != nil {
		return
	}

	id := c.uid()
	ts := c.now()

	switch m := msg.(type) {
	case Alias:
		m.Type = "alias"
		m.MessageId = makeMessageId(m.MessageId, id)
		m.OriginalTimestamp = makeTimestamp(m.OriginalTimestamp, ts)
		m.SentAt = m.OriginalTimestamp
		m.Context = makeContext(m.Context)
		m.Channel = "server"
		msg = m

	case Group:
		m.Type = "group"
		m.MessageId = makeMessageId(m.MessageId, id)
		m.OriginalTimestamp = makeTimestamp(m.OriginalTimestamp, ts)
		m.SentAt = m.OriginalTimestamp
		m.AnonymousId = makeAnonymousId(m.AnonymousId)
		m.Context = makeContext(m.Context)
		m.Channel = "server"
		msg = m

	case Identify:
		m.Type = "identify"
		m.MessageId = makeMessageId(m.MessageId, id)
		m.OriginalTimestamp = makeTimestamp(m.OriginalTimestamp, ts)
		m.SentAt = m.OriginalTimestamp
		m.Context = makeContext(m.Context)
		m.Channel = "server"
		msg = m

	case Page:
		m.Type = "page"
		m.MessageId = makeMessageId(m.MessageId, id)
		m.OriginalTimestamp = makeTimestamp(m.OriginalTimestamp, ts)
		m.SentAt = m.OriginalTimestamp
		m.AnonymousId = makeAnonymousId(m.AnonymousId)
		m.Context = makeContext(m.Context)
		m.Channel = "server"
		msg = m

	case Screen:
		m.Type = "screen"
		m.MessageId = makeMessageId(m.MessageId, id)
		m.OriginalTimestamp = makeTimestamp(m.OriginalTimestamp, ts)
		m.SentAt = m.OriginalTimestamp
		m.AnonymousId = makeAnonymousId(m.AnonymousId)
		m.Context = makeContext(m.Context)
		m.Channel = "server"
		msg = m

	case Track:
		m.Type = "track"
		m.MessageId = makeMessageId(m.MessageId, id)
		m.OriginalTimestamp = makeTimestamp(m.OriginalTimestamp, ts)
		m.SentAt = m.OriginalTimestamp
		m.AnonymousId = makeAnonymousId(m.AnonymousId)
		m.Context = makeContext(m.Context)
		m.Channel = "server"
		msg = m

	default:
		err = fmt.Errorf("messages with custom types cannot be enqueued: %T", msg)
		return
	}

	defer func() {
		// When the `msgs` channel is closed writing to it will trigger a panic.
		// To avoid letting the panic propagate to the caller we recover from it
		// and instead report that the client has been closed and shouldn't be
		// used anymore.
		if recover() != nil {
			err = ErrClosed
		}
	}()

	c.msgs <- msg
	return
}

// Close and flush metrics.
func (c *client) Close() (err error) {
	defer func() {
		// Always recover, a panic could be raised if `c`.quit was closed which
		// means the method was called more than once.
		if recover() != nil {
			err = ErrClosed
		}
	}()
	close(c.quit)
	<-c.shutdown
	return
}

// Asychronously send a batched requests.
func (c *client) sendAsync(msgs []message, wg *sync.WaitGroup, ex *executor) {
	wg.Add(1)

	if !ex.do(func() {
		defer wg.Done()
		defer func() {
			// In case a bug is introduced in the send function that triggers
			// a panic, we don't want this to ever crash the application so we
			// catch it here and log it instead.
			if err := recover(); err != nil {
				c.errorf("panic - %s", err)
			}
		}()
		c.send(msgs, 0)
	}) {
		wg.Done()
		c.errorf("sending messages failed - %s", ErrTooManyRequests)
		c.notifyFailure(msgs, ErrTooManyRequests)
	}
}

// Split based on Anonymous ID
func (c *client) getNodePayload(msgs []message) map[int][]message {
	nodePayload := make(map[int][]message)
	totalNodes := c.totalNodes
	for _, msg := range msgs {
		var req batchRequest
		if err := json.Unmarshal(msg.json, &req); err != nil {
			c.errorf("failed to parse message payload: %v", err)
			continue
		}
		rudderId := req.UserID + ":" + req.AnonymousID
		hashInt := crc32.ChecksumIEEE([]byte(rudderId))
		nodePayload[int(hashInt)%totalNodes] = append(nodePayload[int(hashInt)%totalNodes], msg)
	}
	return nodePayload
}

/*
In the nodepayload , we have sent the payloads till the nodeValue k,
So we get the payloads for remaining nodes to recompuute the nodePayload
based on the new targetNodes
*/
func (c *client) getRevisedMsgs(nodePayload map[int][]message, startFrom int) []message {
	msgs := make([]message, 0)
	for k, v := range nodePayload {
		if k >= startFrom {
			msgs = append(msgs, v...)
		}
	}
	return msgs
}

func (c *client) setNodeCount() {
	const attempts = 10
	for i := 0; i < attempts; i++ {
		if i != 0 {
			time.Sleep(200 * time.Millisecond)
		}
		ci, err := c.clusterInfo()
		if err != nil {
			c.errorf("fetching clusterInfo attempt #%d failed: %v", i+1, err)
			continue
		}
		c.totalNodes = ci.NodeCount
		return
	}
}

func (c *client) clusterInfo() (*clusterInfoResponse, error) {
	url := c.Endpoint + "/cluster-info"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request - %w", err)
	}

	req.Header.Add("User-Agent", "analytics-go (version: "+Version+")")
	req.SetBasicAuth(c.key, "")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request - %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("got response code %s", res.Status)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read cluster-info response: %w", err)
	}
	var ci clusterInfoResponse
	if err := json.Unmarshal(body, &ci); err != nil {
		return nil, fmt.Errorf("failed to parse cluster-info response: %w", err)
	}
	return &ci, nil
}

func (c *client) getMarshalled(msgs []message) ([]byte, error) {
	nodeBatch, err := json.Marshal(batch{
		Messages: msgs,
	})
	return nodeBatch, err
}

// Send batch request.
func (c *client) send(msgs []message, retryAttempt int) {
	const attempts = 10

	ts := c.now()
	for i := range msgs {
		err := msgs[i].setSentAt(ts)
		if err != nil {
			c.errorf("%s - %v", err, msgs[i].msg)
			c.notifyFailure([]message{msgs[i]}, err)
		}
	}

	nodePayload := c.getNodePayload(msgs)
	for k, b := range nodePayload {
		for i := 0; i != attempts; i++ {
			// Get Node Count from Client
			if c.totalNodes == 0 {
				/*
					Since we are running the setNodeCount in a seperate goroutine from the main thread,  we should not send out any packets till
					we have atleast one API call made and totalNodes are set to 1.If the proxy server takes more time to send the response
					we skip this attempt and move to the next attempt.
				*/
				continue
			}
			targetNode := strconv.Itoa(k % c.totalNodes)
			marshalB, err := c.getMarshalled(b)
			if err != nil {
				c.errorf("marshalling messages - %s", err)
				c.notifyFailure(b, err)
				break
			}
			err = c.upload(marshalB, targetNode) // change the names of errors?
			if err == nil {
				c.notifySuccess(b)
				break
			} else if err.Error() == "451" {
				/*In case we have a scaleup/scaledown in the kubernetes nodes, We would recieve a status code of 451 from the Proxy server
				We would then reset the node count by making a call to configure-info end point, then regenerate the payload at a node level
				for only those nodes where we failed in sending the data and then recursively call the send function with the updated payload.
				*/
				const maxSleepTime = 300 * time.Second
				sleepTimeOut := time.Duration(retryAttempt*5) * time.Second
				if sleepTimeOut > maxSleepTime {
					sleepTimeOut = maxSleepTime
				}
				if sleepTimeOut > 0 {
					c.debugf("Retrying in %d seconds", int(sleepTimeOut.Seconds()))
					time.Sleep(sleepTimeOut)
				}
				c.setNodeCount()
				newMsgs := c.getRevisedMsgs(nodePayload, k)
				retryAttempt += 1
				c.send(newMsgs, retryAttempt)
				return
			}
			if i == attempts-1 {
				c.errorf("%d messages dropped because they failed to be sent after %d attempts", len(b), attempts)
				c.notifyFailure(b, err)
			}
			// Wait for either a retry timeout or the client to be closed.
			select {
			case <-time.After(c.RetryAfter(i)):
			case <-c.quit:
				c.errorf("%d messages dropped because they failed to be sent and the client was closed", len(b))
				c.notifyFailure(b, err)
				return
			}
		}
	}
}

// Upload serialized batch message.
func (c *client) upload(b []byte, targetNode string) error {
	url := c.Endpoint + "/v1/batch"
	var (
		req      *http.Request
		reqError error
	)
	if !c.Config.DisableGzip {
		gzipPayload := func(data []byte) (io.Reader, error) {
			var b bytes.Buffer
			gz, err := gzip.NewWriterLevel(&b, gzip.BestSpeed)
			if err != nil {
				return nil, err
			}
			if _, err := gz.Write(data); err != nil {
				return nil, err
			}
			if err = gz.Close(); err != nil {
				return nil, err
			}
			return &b, nil
		}

		payload, err := gzipPayload(b)
		if err != nil {
			c.errorf("gzip payload - %s", err)
			return err
		}
		req, reqError = http.NewRequest("POST", url, payload)
		req.Header.Add("Content-Encoding", "gzip")
	} else {
		req, reqError = http.NewRequest("POST", url, bytes.NewReader(b))
	}

	if reqError != nil {
		c.errorf("creating request - %s", reqError)
		return reqError
	}

	req.Header.Add("User-Agent", "analytics-go (version: "+Version+")")
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Content-Length", strconv.Itoa(len(b)))
	if !c.NoProxySupport {
		req.Header.Add("RS-targetNode", targetNode)
		req.Header.Add("RS-nodeCount", strconv.Itoa(c.totalNodes))
		req.Header.Add("RS-userAgent", "serverSDK")
	}
	req.SetBasicAuth(c.key, "")

	res, err := c.http.Do(req)
	if err != nil {
		c.errorf("sending request - %s", err)
		return err
	}

	defer res.Body.Close()
	return c.report(res)
}

// Report on response body.
func (c *client) report(res *http.Response) (err error) {
	var body []byte
	if res.StatusCode < 300 {
		c.debugf("response %s", res.Status)
		return
	}

	if res.StatusCode == 451 {
		return errors.New(strconv.Itoa(res.StatusCode))
	}

	if body, err = io.ReadAll(res.Body); err != nil {
		c.errorf("response %d %s - %s", res.StatusCode, res.Status, err)
		return
	}

	c.logf("response %d %s – %s", res.StatusCode, res.Status, string(body))
	return fmt.Errorf("%d %s", res.StatusCode, res.Status)
}

// Batch loop.
func (c *client) loop() {
	defer close(c.shutdown)

	wg := &sync.WaitGroup{}
	defer wg.Wait()

	tick := time.NewTicker(c.Interval)
	defer tick.Stop()

	ex := newExecutor(c.maxConcurrentRequests)
	defer ex.close()

	mq := messageQueue{
		maxBatchSize:  c.BatchSize,
		maxBatchBytes: c.maxBatchBytes(),
	}

	for {
		select {
		case msg := <-c.msgs:
			c.push(&mq, msg, wg, ex)

		case <-tick.C:
			c.flush(&mq, wg, ex)

		case <-c.quit:
			c.debugf("exit requested – draining messages")

			// Drain the msg channel, we have to close it first so no more
			// messages can be pushed and otherwise the loop would never end.
			close(c.msgs)
			for msg := range c.msgs {
				c.push(&mq, msg, wg, ex)
			}

			c.flush(&mq, wg, ex)
			c.debugf("exit")
			return
		}
	}
}

func (c *client) push(q *messageQueue, m Message, wg *sync.WaitGroup, ex *executor) {
	var msg message
	var err error

	if msg, err = makeMessage(m, c.MaxMessageBytes); err != nil {
		c.errorf("%s - %v", err, m)
		c.notifyFailure([]message{{m, nil}}, err)
		return
	}

	c.debugf("buffer (%d/%d) %v", len(q.pending), c.BatchSize, m)

	if msgs := q.push(msg); msgs != nil {
		c.debugf("exceeded messages batch limit with batch of %d messages – flushing", len(msgs))
		c.sendAsync(msgs, wg, ex)
	}
}

func (c *client) flush(q *messageQueue, wg *sync.WaitGroup, ex *executor) {
	if msgs := q.flush(); msgs != nil {
		c.debugf("flushing %d messages", len(msgs))
		c.sendAsync(msgs, wg, ex)
	}
}

func (c *client) debugf(format string, args ...interface{}) {
	if c.Verbose {
		c.logf(format, args...)
	}
}

func (c *client) logf(format string, args ...interface{}) {
	c.Logger.Logf(format, args...)
}

func (c *client) errorf(format string, args ...interface{}) {
	c.Logger.Errorf(format, args...)
}

func (c *client) maxBatchBytes() int {
	b, _ := json.Marshal(batch{})
	return c.MaxBatchBytes - len(b)
}

func (c *client) notifySuccess(msgs []message) {
	if c.Callback != nil {
		for _, m := range msgs {
			c.Callback.Success(m.msg)
		}
	}
}

func (c *client) notifyFailure(msgs []message, err error) {
	if c.Callback != nil {
		for _, m := range msgs {
			c.Callback.Failure(m.msg, err)
		}
	}
}
