package client // import "github.com/cocoonlife/influxdb/client/v2"

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/cocoonlife/influxdb/models"
	"github.com/ncw/rclone/crypt/pkcs7"
)

// UDPPayloadSize is a reasonable default payload size for UDP packets that
// could be travelling over the internet.
const (
	UDPPayloadSize = 512
)

// HTTPConfig is the config data needed to create an HTTP Client
type HTTPConfig struct {
	// Addr should be of the form "http://host:port"
	// or "http://[ipv6-host%zone]:port".
	Addr string

	// Username is the influxdb username, optional
	Username string

	// Password is the influxdb password, optional
	Password string

	// UserAgent is the http User Agent, defaults to "InfluxDBClient"
	UserAgent string

	// Timeout for influxdb writes, defaults to no timeout
	Timeout time.Duration

	// InsecureSkipVerify gets passed to the http client, if true, it will
	// skip https certificate verification. Defaults to false
	InsecureSkipVerify bool

	// TLSConfig allows the user to set their own TLS config for the HTTP
	// Client. If set, this option overrides InsecureSkipVerify.
	TLSConfig *tls.Config
}

// UDPConfig is the config data needed to create a UDP Client
type UDPConfig struct {
	// Addr should be of the form "host:port"
	// or "[ipv6-host%zone]:port".
	Addr string

	// PayloadSize is the maximum size of a UDP client message, optional
	// Tune this based on your network. Defaults to UDPBufferSize.
	PayloadSize int
}

// BatchPointsConfig is the config data needed to create an instance of the BatchPoints struct
type BatchPointsConfig struct {
	// Precision is the write precision of the points, defaults to "ns"
	Precision string

	// Database is the database to write points to
	Database string

	// RetentionPolicy is the retention policy of the points
	RetentionPolicy string

	// Write consistency is the number of servers required to confirm write
	WriteConsistency string
}

// Client is a client interface for writing & querying the database
type Client interface {
	// Ping checks that status of cluster
	Ping(timeout time.Duration) (time.Duration, string, error)

	// Write takes a BatchPoints object and writes all Points to InfluxDB.
	Write(bp BatchPoints) error

	// Query makes an InfluxDB Query on the database. This will fail if using
	// the UDP client.
	Query(q Query) (*Response, error)

	// Close releases any resources a Client may be using.
	Close() error
}

// NewHTTPClient returns a new Client from the provided config.
// Client is safe for concurrent use by multiple goroutines.
func NewHTTPClient(conf HTTPConfig) (Client, error) {
	if conf.UserAgent == "" {
		conf.UserAgent = "InfluxDBClient"
	}

	u, err := url.Parse(conf.Addr)
	if err != nil {
		return nil, err
	} else if u.Scheme != "http" && u.Scheme != "https" {
		m := fmt.Sprintf("Unsupported protocol scheme: %s, your address"+
			" must start with http:// or https://", u.Scheme)
		return nil, errors.New(m)
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: conf.InsecureSkipVerify,
		},
	}
	if conf.TLSConfig != nil {
		tr.TLSClientConfig = conf.TLSConfig
	}
	return &client{
		url:       *u,
		username:  conf.Username,
		password:  conf.Password,
		useragent: conf.UserAgent,
		httpClient: &http.Client{
			Timeout:   conf.Timeout,
			Transport: tr,
		},
		transport: tr,
	}, nil
}

// Ping will check to see if the server is up with an optional timeout on waiting for leader.
// Ping returns how long the request took, the version of the server it connected to, and an error if one occurred.
func (c *client) Ping(timeout time.Duration) (time.Duration, string, error) {
	now := time.Now()
	u := c.url
	u.Path = "ping"

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return 0, "", err
	}

	req.Header.Set("User-Agent", c.useragent)

	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	if timeout > 0 {
		params := req.URL.Query()
		params.Set("wait_for_leader", fmt.Sprintf("%.0fs", timeout.Seconds()))
		req.URL.RawQuery = params.Encode()
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}

	if resp.StatusCode != http.StatusNoContent {
		var err = fmt.Errorf(string(body))
		return 0, "", err
	}

	version := resp.Header.Get("X-Influxdb-Version")
	return time.Since(now), version, nil
}

// Close releases the client's resources.
func (c *client) Close() error {
	c.transport.CloseIdleConnections()
	return nil
}

// NewUDPClient returns a client interface for writing to an InfluxDB UDP
// service from the given config.
func NewUDPClient(conf UDPConfig) (Client, error) {
	var udpAddr *net.UDPAddr
	udpAddr, err := net.ResolveUDPAddr("udp", conf.Addr)
	if err != nil {
		return nil, err
	}

	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return nil, err
	}

	payloadSize := conf.PayloadSize
	if payloadSize == 0 {
		payloadSize = UDPPayloadSize
	}

	return &udpclient{
		conn:        conn,
		payloadSize: payloadSize,
	}, nil
}

// Ping will check to see if the server is up with an optional timeout on waiting for leader.
// Ping returns how long the request took, the version of the server it connected to, and an error if one occurred.
func (uc *udpclient) Ping(timeout time.Duration) (time.Duration, string, error) {
	return 0, "", nil
}

// Close releases the udpclient's resources.
func (uc *udpclient) Close() error {
	return uc.conn.Close()
}

// client is safe for concurrent use as the fields are all read-only
// once the client is instantiated.
type client struct {
	// N.B - if url.UserInfo is accessed in future modifications to the
	// methods on client, you will need to syncronise access to url.
	url        url.URL
	username   string
	password   string
	useragent  string
	httpClient *http.Client
	transport  *http.Transport
}

type udpclient struct {
	conn        *net.UDPConn
	payloadSize int
}

// BatchPoints is an interface into a batched grouping of points to write into
// InfluxDB together. BatchPoints is NOT thread-safe, you must create a separate
// batch for each goroutine.
type BatchPoints interface {
	// AddPoint adds the given point to the Batch of points
	AddPoint(p *Point)
	// AddPoints adds the given points to the Batch of points
	AddPoints(ps []*Point)
	// Points lists the points in the Batch
	Points() []*Point

	// Precision returns the currently set precision of this Batch
	Precision() string
	// SetPrecision sets the precision of this batch.
	SetPrecision(s string) error

	// Database returns the currently set database of this Batch
	Database() string
	// SetDatabase sets the database of this Batch
	SetDatabase(s string)

	// WriteConsistency returns the currently set write consistency of this Batch
	WriteConsistency() string
	// SetWriteConsistency sets the write consistency of this Batch
	SetWriteConsistency(s string)

	// RetentionPolicy returns the currently set retention policy of this Batch
	RetentionPolicy() string
	// SetRetentionPolicy sets the retention policy of this Batch
	SetRetentionPolicy(s string)
}

// NewBatchPoints returns a BatchPoints interface based on the given config.
func NewBatchPoints(conf BatchPointsConfig) (BatchPoints, error) {
	if conf.Precision == "" {
		conf.Precision = "ns"
	}
	if _, err := time.ParseDuration("1" + conf.Precision); err != nil {
		return nil, err
	}
	bp := &batchpoints{
		database:         conf.Database,
		precision:        conf.Precision,
		retentionPolicy:  conf.RetentionPolicy,
		writeConsistency: conf.WriteConsistency,
	}
	return bp, nil
}

type batchpoints struct {
	points           []*Point
	database         string
	precision        string
	retentionPolicy  string
	writeConsistency string
}

func (bp *batchpoints) AddPoint(p *Point) {
	bp.points = append(bp.points, p)
}

func (bp *batchpoints) AddPoints(ps []*Point) {
	bp.points = append(bp.points, ps...)
}

func (bp *batchpoints) Points() []*Point {
	return bp.points
}

func (bp *batchpoints) Precision() string {
	return bp.precision
}

func (bp *batchpoints) Database() string {
	return bp.database
}

func (bp *batchpoints) WriteConsistency() string {
	return bp.writeConsistency
}

func (bp *batchpoints) RetentionPolicy() string {
	return bp.retentionPolicy
}

func (bp *batchpoints) SetPrecision(p string) error {
	if _, err := time.ParseDuration("1" + p); err != nil {
		return err
	}
	bp.precision = p
	return nil
}

func (bp *batchpoints) SetDatabase(db string) {
	bp.database = db
}

func (bp *batchpoints) SetWriteConsistency(wc string) {
	bp.writeConsistency = wc
}

func (bp *batchpoints) SetRetentionPolicy(rp string) {
	bp.retentionPolicy = rp
}

// Point represents a single data point
type Point struct {
	pt models.Point
}

// NewPoint returns a point with the given timestamp. If a timestamp is not
// given, then data is sent to the database without a timestamp, in which case
// the server will assign local time upon reception. NOTE: it is recommended to
// send data with a timestamp.
func NewPoint(
	name string,
	tags map[string]string,
	fields map[string]interface{},
	t ...time.Time,
) (*Point, error) {
	var T time.Time
	if len(t) > 0 {
		T = t[0]
	}

	pt, err := models.NewPoint(name, tags, fields, T)
	if err != nil {
		return nil, err
	}
	return &Point{
		pt: pt,
	}, nil
}

// String returns a line-protocol string of the Point
func (p *Point) String() string {
	return p.pt.String()
}

// PrecisionString returns a line-protocol string of the Point, at precision
func (p *Point) PrecisionString(precison string) string {
	return p.pt.PrecisionString(precison)
}

// Name returns the measurement name of the point
func (p *Point) Name() string {
	return p.pt.Name()
}

// Tags returns the tags associated with the point
func (p *Point) Tags() map[string]string {
	return p.pt.Tags()
}

// Time return the timestamp for the point
func (p *Point) Time() time.Time {
	return p.pt.Time()
}

// UnixNano returns the unix nano time of the point
func (p *Point) UnixNano() int64 {
	return p.pt.UnixNano()
}

// Fields returns the fields for the point
func (p *Point) Fields() map[string]interface{} {
	return p.pt.Fields()
}

// NewPointFrom returns a point from the provided models.Point.
func NewPointFrom(pt models.Point) *Point {
	return &Point{pt: pt}
}

func (uc *udpclient) Write(bp BatchPoints) error {
	var b bytes.Buffer
	var d time.Duration
	d, _ = time.ParseDuration("1" + bp.Precision())

	for _, p := range bp.Points() {
		pointstring := p.pt.RoundedString(d) + "\n"

		// Write and reset the buffer if we reach the max size
		if b.Len()+len(pointstring) >= uc.payloadSize {
			if _, err := uc.conn.Write(b.Bytes()); err != nil {
				return err
			}
			b.Reset()
		}

		if _, err := b.WriteString(pointstring); err != nil {
			return err
		}
	}

	_, err := uc.conn.Write(b.Bytes())
	return err
}

func (c *client) Write(bp BatchPoints) error {
	var b bytes.Buffer

	for _, p := range bp.Points() {
		if _, err := b.WriteString(p.pt.PrecisionString(bp.Precision())); err != nil {
			return err
		}

		if err := b.WriteByte('\n'); err != nil {
			return err
		}
	}

	u := c.url
	u.Path = "write"
	req, err := http.NewRequest("POST", u.String(), &b)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "")
	req.Header.Set("User-Agent", c.useragent)
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	params := req.URL.Query()
	params.Set("db", bp.Database())
	params.Set("rp", bp.RetentionPolicy())
	params.Set("precision", bp.Precision())
	params.Set("consistency", bp.WriteConsistency())
	req.URL.RawQuery = params.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		var err = fmt.Errorf(string(body))
		return err
	}

	return nil
}

// Query defines a query to send to the server
type Query struct {
	Command   string
	Database  string
	Precision string
}

// NewQuery returns a query object
// database and precision strings can be empty strings if they are not needed
// for the query.
func NewQuery(command, database, precision string) Query {
	return Query{
		Command:   command,
		Database:  database,
		Precision: precision,
	}
}

// Response represents a list of statement results.
type Response struct {
	Results []Result
	Err     string `json:"error,omitempty"`
}

// Error returns the first error from any statement.
// Returns nil if no errors occurred on any statements.
func (r *Response) Error() error {
	if r.Err != "" {
		return fmt.Errorf(r.Err)
	}
	for _, result := range r.Results {
		if result.Err != "" {
			return fmt.Errorf(result.Err)
		}
	}
	return nil
}

// Message represents a user message.
type Message struct {
	Level string
	Text  string
}

// Result represents a resultset returned from a single statement.
type Result struct {
	Series   []models.Row
	Messages []*Message
	Err      string `json:"error,omitempty"`
}

func (uc *udpclient) Query(q Query) (*Response, error) {
	return nil, fmt.Errorf("Querying via UDP is not supported")
}

// Query sends a command to the server and returns the Response
func (c *client) Query(q Query) (*Response, error) {
	u := c.url
	u.Path = "query"

	req, err := http.NewRequest("POST", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "")
	req.Header.Set("User-Agent", c.useragent)
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	params := req.URL.Query()
	params.Set("q", q.Command)
	params.Set("db", q.Database)
	if q.Precision != "" {
		params.Set("epoch", q.Precision)
	}
	req.URL.RawQuery = params.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var response Response
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	decErr := dec.Decode(&response)

	// ignore this error if we got an invalid status code
	if decErr != nil && decErr.Error() == "EOF" && resp.StatusCode != http.StatusOK {
		decErr = nil
	}
	// If we got a valid decode error, send that back
	if decErr != nil {
		return nil, fmt.Errorf("unable to decode json: received status code %d err: %s", resp.StatusCode, decErr)
	}
	// If we don't have an error in our json response, and didn't get statusOK
	// then send back an error
	if resp.StatusCode != http.StatusOK && response.Error() == nil {
		return &response, fmt.Errorf("received status code %d from server",
			resp.StatusCode)
	}
	return &response, nil
}

// EncUDPConfig is the config data needed to create an encrypted UDP Client
type EncUDPConfig struct {
	// Addr should be of the form "host:port"
	// or "[ipv6-host%zone]:port".
	Addr string

	// PayloadSize is the maximum size of a UDP client message, optional
	// Tune this based on your network. Defaults to UDPBufferSize.
	PayloadSize int

	// Encoding is the encoding type
	Encoding int

	// Key is the key to encrypt the data with
	Key []byte
}

type encudpclient struct {
	conn        *net.UDPConn
	payloadSize int
	encoding    int
	key         []byte
}

// NewEncUDPClient returns a client interface for writing to an InfluxDB UDP
// service with encryption from the given config.
func NewEncUDPClient(conf EncUDPConfig) (Client, error) {
	var udpAddr *net.UDPAddr
	udpAddr, err := net.ResolveUDPAddr("udp", conf.Addr)
	if err != nil {
		return nil, err
	}

	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return nil, err
	}

	payloadSize := conf.PayloadSize
	if payloadSize == 0 {
		payloadSize = UDPPayloadSize
	}

	// Make room for CRC
	payloadSize -= 4

	// Make room for IV
	payloadSize -= 16

	// Make room for padding
        payloadSize -= 16

	return &encudpclient{
		conn:        conn,
		payloadSize: payloadSize,
		encoding:    conf.Encoding,
		key:         conf.Key,
	}, nil
}

// Ping will check to see if the server is up with an optional timeout on waiting for leader.
// Ping returns how long the request took, the version of the server it connected to, and an error if one occurred.
func (euc *encudpclient) Ping(timeout time.Duration) (time.Duration, string, error) {
	return 0, "", nil
}

// Close releases the encudpclient's resources.
func (euc *encudpclient) Close() error {
	return euc.conn.Close()
}

func encodeData(data, key, iv []byte) ([]byte, error) {
	var b bytes.Buffer
	csum := crc32.Update(0, crc32.IEEETable, data)
	err := binary.Write(&b, binary.LittleEndian, csum)
	if err != nil {
		return nil, err
	}
	_, err = b.Write(data)
	if err != nil {
		return nil, err
	}
	paddedData := pkcs7.Pad(aes.BlockSize, b.Bytes())
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	header := "IDB\x01"
	ciphertext := make([]byte, len(header)+aes.BlockSize+len(paddedData))
	copy(ciphertext, header)
	copy(ciphertext[len(header):], iv)

	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(ciphertext[len(header)+aes.BlockSize:], paddedData)
	return ciphertext, nil
}

func (euc *encudpclient) writeData(data []byte) (int, error) {
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return 0, err
	}
	ciphertext, err := encodeData(data, euc.key, iv)
	if err != nil {
		return 0, err
	}
	return euc.conn.Write(ciphertext)
}

func (euc *encudpclient) Write(bp BatchPoints) error {
	var b bytes.Buffer
	var d time.Duration
	d, _ = time.ParseDuration("1" + bp.Precision())

	for _, p := range bp.Points() {
		pointstring := p.pt.RoundedString(d) + "\n"

		// Write and reset the buffer if we reach the max size
		if b.Len()+len(pointstring) >= euc.payloadSize {
			if _, err := euc.writeData(b.Bytes()); err != nil {
				return err
			}
			b.Reset()
		}

		if _, err := b.WriteString(pointstring); err != nil {
			return err
		}
	}

	_, err := euc.writeData(b.Bytes())
	return err
}

func (euc *encudpclient) Query(q Query) (*Response, error) {
	return nil, fmt.Errorf("Querying via UDP is not supported")
}
