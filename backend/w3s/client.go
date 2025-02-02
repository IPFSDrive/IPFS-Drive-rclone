package w3s

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/alanshaw/go-carbites"
	"github.com/filecoin-project/go-address"
	"github.com/ipfs/go-blockservice"
	bserv "github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	ds "github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/ipfs/go-merkledag"
	"github.com/ipfs/ipfs-cluster/api"
	"github.com/ipld/go-car"
	peer "github.com/libp2p/go-libp2p-core/peer"
	"github.com/rclone/rclone/backend/w3s/adder"
	w3http "github.com/rclone/rclone/backend/w3s/http"
	rcfs "github.com/rclone/rclone/fs"
)

const targetChunkSize = 1024 * 1024 * 10
const iso8601 = "2006-01-02T15:04:05Z0700"

// Client is a HTTP API client to the web3.storage service.
type Client interface {
	Get(context.Context, cid.Cid) (*w3http.Web3Response, error)
	GetUsingIpfs(context.Context, cid.Cid) (*w3http.Web3Response, error)
	//从ipfs直接下载文件对象，要求cid是文件
	GetIpfsFile(context.Context, cid.Cid) (*w3http.Web3Response, error)

	Put(context.Context, fs.File, ...PutOption) (cid.Cid, error)
	PutCar(context.Context, io.Reader) (cid.Cid, error)
	Status(context.Context, cid.Cid) (*Status, error)
	StatusIpfs(ctx context.Context, cid2 cid.Cid) (*IpfsStatus, error)
	List(ctx context.Context) (entries []MyStatusJson, err error)

	PutRclone(ctx context.Context, in io.Reader, src rcfs.ObjectInfo, cfg putConfig) (cid.Cid, error)
}

type PinStatus int

const (
	PinStatusPinned    = PinStatus(api.TrackerStatusPinned)
	PinStatusPinning   = PinStatus(api.TrackerStatusPinning)
	PinStatusPinQueued = PinStatus(api.TrackerStatusPinQueued)
)

func (s PinStatus) String() string {
	if s == PinStatusPinned {
		return "Pinned"
	}
	if s == PinStatusPinning {
		return "Pinning"
	}
	if s == PinStatusPinQueued {
		return "PinQueued"
	}
	return "Unknown"
}

type Pin struct {
	PeerID   peer.ID
	PeerName string
	Region   string
	Status   PinStatus
	Updated  time.Time
}

type pinJson struct {
	PeerID   string `json:"peerId"`
	PeerName string `json:"peerName"`
	Region   string `json:"region"`
	Status   string `json:"status"`
	Updated  string `json:"updated"`
}

func (p *Pin) UnmarshalJSON(b []byte) error {
	var raw pinJson
	err := json.Unmarshal(b, &raw)
	if err != nil {
		return err
	}
	p.PeerID, err = peer.Decode(raw.PeerID)
	if err != nil {
		return err
	}
	p.PeerName = raw.PeerName
	p.Region = raw.Region
	if raw.Status == "Pinned" {
		p.Status = PinStatusPinned
	} else if raw.Status == "Pinning" {
		p.Status = PinStatusPinning
	} else if raw.Status == "PinQueued" {
		p.Status = PinStatusPinQueued
	} else {
		return fmt.Errorf("unknown deal status: %s", raw.Status)
	}
	return nil
}

type DealStatus int

const (
	DealStatusQueued DealStatus = iota
	DealStatusPublished
	DealStatusActive
)

func (s DealStatus) String() string {
	return []string{"Queued", "Published", "Active"}[s]
}

type Deal struct {
	DealID            uint64
	StorageProvider   address.Address
	Status            DealStatus
	PieceCid          cid.Cid
	DataCid           cid.Cid
	DataModelSelector string
	Activation        time.Time
	Created           time.Time
	Updated           time.Time
}

type dealJson struct {
	DealID            uint64 `json:"dealId,omitempty"`
	StorageProvider   string `json:"storageProvider,omitempty"`
	Status            string `json:"status"`
	PieceCid          string `json:"pieceCid,omitempty"`
	DataCid           string `json:"dataCid,omitempty"`
	DataModelSelector string `json:"dataModelSelector,omitempty"`
	Activation        string `json:"activation,omitempty"`
	Created           string `json:"created"`
	Updated           string `json:"updated"`
}

func (d *Deal) UnmarshalJSON(b []byte) error {
	var raw dealJson
	err := json.Unmarshal(b, &raw)
	if err != nil {
		return err
	}
	d.DealID = raw.DealID
	d.StorageProvider, err = address.NewFromString(raw.StorageProvider)
	if err != nil {
		return err
	}
	if raw.Status == "Queued" {
		d.Status = DealStatusQueued
	} else if raw.Status == "Published" {
		d.Status = DealStatusPublished
	} else if raw.Status == "Active" {
		d.Status = DealStatusActive
	} else {
		return fmt.Errorf("unknown deal status: %s", raw.Status)
	}
	if raw.PieceCid != "" {
		d.PieceCid, err = cid.Parse(raw.PieceCid)
		if err != nil {
			return err
		}
	} else {
		d.PieceCid = cid.Undef
	}
	if raw.DataCid != "" {
		d.DataCid, err = cid.Parse(raw.DataCid)
		if err != nil {
			return err
		}
	} else {
		d.DataCid = cid.Undef
	}
	d.DataModelSelector = raw.DataModelSelector
	if raw.Activation != "" {
		d.Activation, err = time.Parse(iso8601, raw.Activation)
		if err != nil {
			return err
		}
	}
	d.Created, err = time.Parse(iso8601, raw.Created)
	if err != nil {
		return err
	}
	d.Updated, err = time.Parse(iso8601, raw.Updated)
	if err != nil {
		return err
	}
	return nil
}

// Status is IPFS pin and Filecoin deal status for a given CID.
type Status struct {
	Cid     cid.Cid
	DagSize uint64
	Created time.Time
	Pins    []Pin
	Deals   []Deal
}

type IpfsStatus struct {
	Data  string `json:"data"`
	Links []Link `json:"links"`
}
type Link struct {
	Name string `json:"Name"`
	Size int64
	Cid  cid.Cid
}
type statusJson struct {
	Cid     string `json:"cid"`
	DagSize uint64 `json:"dagSize"`
	Created string `json:"created"`
	Pins    []Pin  `json:"pins"`
	Deals   []Deal `json:"deals"`
}

type MyStatusJson struct {
	Name    string `json:"name"`
	Cid     string `json:"cid"`
	DagSize uint64 `json:"dagSize"`
	Created string `json:"created"`
	//Pins    []Pin  `json:"pins"`
	//Deals   []Deal `json:"deals"`
}

func (s *Status) UnmarshalJSON(b []byte) error {
	var raw statusJson
	err := json.Unmarshal(b, &raw)
	if err != nil {
		return err
	}
	s.Cid, err = cid.Parse(raw.Cid)
	if err != nil {
		return err
	}
	s.DagSize = raw.DagSize
	s.Created, err = time.Parse(iso8601, raw.Created)
	if err != nil {
		return err
	}
	s.Pins = raw.Pins
	s.Deals = raw.Deals
	return nil
}

type clientConfig struct {
	token        string
	endpoint     string
	ipfsEndpoint string
	ds           ds.Batching
}

type client struct {
	cfg  *clientConfig
	bsvc blockservice.BlockService
	hc   *http.Client
}

// NewClient creates a new web3.storage API client.
func NewClient(options ...Option) (Client, error) {
	cfg := clientConfig{
		endpoint:     "https://api.web3.storage",
		ipfsEndpoint: "http://dweb.link",
	}
	for _, opt := range options {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}
	if cfg.token == "" {
		return nil, fmt.Errorf("missing auth token")
	}
	c := client{cfg: &cfg, hc: &http.Client{}}
	if cfg.ds != nil {
		c.bsvc = bserv.New(blockstore.NewBlockstore(cfg.ds), nil)
	} else {
		ds := dssync.MutexWrap(ds.NewMapDatastore())
		c.bsvc = bserv.New(blockstore.NewBlockstore(ds), nil)
	}
	return &c, nil
}

// TODO: retry
func (c *client) sendCar(ctx context.Context, r io.Reader) (cid.Cid, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.endpoint+"/car", r)
	if err != nil {
		return cid.Undef, err
	}
	req.Header.Add("Content-Type", "application/car")
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", c.cfg.token))
	res, err := c.hc.Do(req)
	if err != nil {
		return cid.Undef, err
	}
	if res.StatusCode != 200 {
		return cid.Undef, fmt.Errorf("unexpected response status: %d", res.StatusCode)
	}
	d := json.NewDecoder(res.Body)
	var out struct {
		Cid string `json:"cid"`
	}
	err = d.Decode(&out)
	if err != nil {
		return cid.Undef, err
	}
	return cid.Parse(out.Cid)
}

func (c *client) Get(ctx context.Context, cid cid.Cid) (*w3http.Web3Response, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/car/%s", c.cfg.endpoint, cid), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", c.cfg.token))
	res, err := c.hc.Do(req)
	return w3http.NewWeb3Response(res, c.bsvc), err
}

func (c *client) GetUsingIpfs(ctx context.Context, cid cid.Cid) (*w3http.Web3Response, error) {
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/api/v0/dag/export?arg=%s", c.cfg.ipfsEndpoint, cid), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", c.cfg.token))
	res, err := c.hc.Do(req)
	return w3http.NewWeb3Response(res, c.bsvc), err
}

func (c *client) GetIpfsFile(ctx context.Context, cid cid.Cid) (*w3http.Web3Response, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://%s.ipfs.dweb.link/", cid), nil)
	if err != nil {
		return nil, err
	}
	res, err := c.hc.Do(req)
	return w3http.NewWeb3Response(res, c.bsvc), err
}

func (c *client) List(ctx context.Context) ([]MyStatusJson, error) {

	req, err := http.NewRequest("GET", fmt.Sprintf("%s/user/uploads", c.cfg.endpoint), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", c.cfg.token))
	res, err := c.hc.Do(req)
	//println(res.Body

	//buf := new(strings.Builder)
	//io.Copy(buf, res.Body)
	//// check errors
	//fmt.Println(buf.String())
	if err != nil {
		return nil, err
	}

	defer res.Body.Close()

	dec := json.NewDecoder(res.Body)

	t, err := dec.Token()
	if err != nil {
		log.Fatal(err)
		fmt.Printf("%T: %v\n", t, t)
	}
	//
	ret := []MyStatusJson{}

	for dec.More() {
		var m MyStatusJson
		// decode an array value (Message)
		err := dec.Decode(&m)
		if err != nil {
			log.Fatal(err)
		}

		ret = append(ret, m)

		//fmt.Printf("%v: %v\n", m.Name, m.Text)
	}
	t, err = dec.Token()
	//if err != nil {
	//	log.Fatal(err)
	//}
	//fmt.Printf("%T: %v\n", t, t)

	return ret, nil

}

type putConfig struct {
	fsys    fs.FS
	dirname string
}

type MyFile struct {
	in  io.Reader
	src rcfs.ObjectInfo
	cfg *putConfig
}
type MyInfo struct {
	src rcfs.ObjectInfo
	cfg *putConfig
}

func (m MyInfo) Name() string {
	return m.src.Remote()
}

func (m MyInfo) Size() int64 {
	return m.src.Size()
}

func (m MyInfo) Mode() fs.FileMode {
	panic("implement me")
}

func (m MyInfo) ModTime() time.Time {
	return m.src.ModTime(context.Background())
}

func (m MyInfo) IsDir() bool {
	return false
}

func (m MyInfo) Sys() interface{} {
	panic("implement me")
}

func (m MyFile) Stat() (fs.FileInfo, error) {
	return MyInfo{src: m.src, cfg: m.cfg}, nil
}

func (m MyFile) Read(bytes []byte) (int, error) {
	return m.in.Read(bytes)
}

func (m MyFile) Close() error {
	return nil
}

type ChangeName struct {
	name string `json:"name"`
}

func (c *client) PutRclone(ctx context.Context, in io.Reader, src rcfs.ObjectInfo, incfg putConfig) (cid.Cid, error) {

	mc, e := c.Put(ctx, MyFile{src: src, in: in, cfg: &incfg}, func(cfg *putConfig) error {
		cfg.dirname = incfg.dirname
		return nil
	})
	if e != nil {
		return cid.Undef, e
	}
	buf := new(bytes.Buffer)
	buf.WriteString(fmt.Sprintf("{\"name\":\"%s\"}", src.Remote()))
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("%s/user/uploads/%s/rename", c.cfg.endpoint, mc.String()), buf)

	println(buf.String())
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", c.cfg.token))
	req.Header.Add("Content-Type", "application/json")
	//req.Header.Add("Authorization", "Bearer WyIweGI5NGNmZjdmNzczOTNmZWMxZTcyZTU1ZTQzZWViMGMwNjM2MmIwZmU2YTNjYjYzOTg3MDJmZjkxODZhOWFmMGYxMmRjYmE1MjVmYjAzNThlOGExZmY1ZjNhYTQ5ZjhmODljNzFhZWEzY2Q3ZmFmMmVkMGQ5YmJkYTVmZGJjNzBjMWIiLCJ7XCJpYXRcIjoxNjMzNDMzNjM3LFwiZXh0XCI6MTYzMzQzNDUzNyxcImlzc1wiOlwiZGlkOmV0aHI6MHg3QTFjZURjZGZjMTliMzE1YTE5NTU2MGNCQTNGNjRCOGM2Njc1NDg2XCIsXCJzdWJcIjpcIkZTUHFOekYtOG5hdUFJWnBaT1lRWmpvOHhndWE1UE1LZG5ZdnhzNzVMTlk9XCIsXCJhdWRcIjpcIm1UUl9NQ3JTTjU3NFVsRmIzUFdyYzYyYllHNFgyczAxemZGcHRJSkkwa2M9XCIsXCJuYmZcIjoxNjMzNDMzNjM3LFwidGlkXCI6XCI3MjgzYjc3Yi1kY2RiLTRjZDgtODg4Yy00MDg0MzAxNTYzOGNcIixcImFkZFwiOlwiMHhhMTc2NWEzODAwNmFhMmMyYmU1Yjk1Yzg3MDVmMDBiNDJlYjBkNDY3Nzc3NzMzM2I1NDJkMmIyMmU5ODkzNzY3NDg2ODEyZmFlMjQzMDVhZWMxNTRmMGQxZjUzN2I1ZWNmYzM5ZDNkMDlmMzBlZmNmY2JjOTk0MjFjNzU5ZTc2MzFiXCJ9Il0=")

	//req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", c.cfg.token))

	res, _ := c.hc.Do(req)

	if res != nil {

		println(res.Status)
		res.Body.Close()
	}

	return mc, nil

}

// Put uploads files to Web3.Storage. The file argument can be a single file or
// a directory. If a directory is passed and the directory does NOT implement
// fs.ReadDirFile then the WithDirname option should be passed (or the current
// process working directory will be used).
func (c *client) Put(ctx context.Context, file fs.File, options ...PutOption) (cid.Cid, error) {
	var cfg putConfig
	for _, opt := range options {
		if err := opt(&cfg); err != nil {
			return cid.Undef, err
		}
	}

	info, err := file.Stat()
	if err != nil {
		return cid.Undef, err
	}

	dag := merkledag.NewDAGService(c.bsvc)
	dagFmtr, err := adder.NewAdder(ctx, dag)
	if err != nil {
		return cid.Undef, err
	}

	root, err := dagFmtr.Add(file, cfg.dirname, cfg.fsys)
	if err != nil {
		return cid.Undef, err
	}

	// If file is a dir, do not wrap in another.
	if info.IsDir() {
		mr, err := dagFmtr.MfsRoot()
		if err != nil {
			return cid.Undef, err
		}
		rdir := mr.GetDirectory()
		cdir, err := rdir.Child(info.Name())
		if err != nil {
			return cid.Undef, err
		}
		cnode, err := cdir.GetNode()
		if err != nil {
			return cid.Undef, err
		}
		root = cnode.Cid()
	}

	// fmt.Println("root CID", root)

	carReader, carWriter := io.Pipe()

	go func() {
		err = car.WriteCar(ctx, dag, []cid.Cid{root}, carWriter)
		if err != nil {
			carWriter.CloseWithError(err)
			return
		}
		carWriter.Close()
	}()

	return c.PutCar(ctx, carReader)
}

// PutCar uploads a CAR (Content Addressable Archive) to Web3.Storage.
func (c *client) PutCar(ctx context.Context, car io.Reader) (cid.Cid, error) {
	carChunks := make(chan io.Reader)

	var root cid.Cid
	var wg sync.WaitGroup
	wg.Add(1)

	var sendErr error
	go func() {
		defer wg.Done()
		for r := range carChunks {
			// TODO: concurrency
			c, err := c.sendCar(ctx, r)
			if err != nil {
				sendErr = err
				break
			}
			root = c
		}
	}()

	err := carbites.Split(ctx, car, targetChunkSize, carbites.Treewalk, carChunks)
	if err != nil {
		return cid.Undef, err
	}
	wg.Wait()

	return root, sendErr
}

func (c *client) Status(ctx context.Context, cid cid.Cid) (*Status, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/status/%s", c.cfg.endpoint, cid), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", c.cfg.token))
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected response status: %d", res.StatusCode)
	}
	var s Status
	d := json.NewDecoder(res.Body)
	err = d.Decode(&s)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *client) StatusIpfs(ctx context.Context, cid cid.Cid) (*IpfsStatus, error) {
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/api/v0/dag/get?arg=%s", c.cfg.ipfsEndpoint, cid), nil)
	if err != nil {
		return nil, err
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected response status: %d", res.StatusCode)
	}
	var s IpfsStatus
	//bs,_:= ioutil.ReadAll(res.Body)
	//println(string(bs))
	d := json.NewDecoder(res.Body)
	err = d.Decode(&s)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

var (
	_ fs.File     = &MyFile{}
	_ fs.FileInfo = &MyInfo{}
)
