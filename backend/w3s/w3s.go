package w3s

import (
	"context"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/hash"
	//"github.com/web3-storage/go-w3s-client"
	"github.com/ipfs/go-cid"
	"io"
	//iofs "io/fs"

	"time"
)

func init() {

	fs.Register(&fs.RegInfo{
		Name:        "w3s",
		Description: "Web3 Storage",
		Prefix:      "w3s",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:     "w3s_token",
			Help:     "Web3 storage token",
			Required: true,
		}, {
			Name:    "w3s_server_url",
			Help:    "Server Url",
			Default: "https://api.web3.storage",
		},
		},
	})
}

// Options defines the configuration for this backend
type Options struct {
	Endpoint string `config:"w3s_server_url"`
	Token    string `config:"w3s_token"`
}

// Fs stores the interface to the remote HTTP files
type Fs struct {
	name     string
	root     string
	features *fs.Features   // optional features
	opt      Options        // options for this backend
	ci       *fs.ConfigInfo // global config
	//endpoint    *url.URL
	//endpointURL string // endpoint as a string
	client *Client
}

func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {

	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	Mclient, err := NewClient(
		WithEndpoint(opt.Endpoint),
		WithToken(opt.Token),
		//w3s.WithEndpoint("https://api.web3.storage"),
		//w3s.WithToken("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJkaWQ6ZXRocjoweDdBMWNlRGNkZmMxOWIzMTVhMTk1NTYwY0JBM0Y2NEI4YzY2NzU0ODYiLCJpc3MiOiJ3ZWIzLXN0b3JhZ2UiLCJpYXQiOjE2MzMwNjIxMzI5MzksIm5hbWUiOiJzeW5jIn0.idpWUdJ0J6bsMdQb8_OMKqgvjvvLGOMqstrVBFLh58M"),
	)
	if err != nil {
		panic(err)
	}
	f := &Fs{
		name:   name,
		root:   root,
		opt:    *opt,
		ci:     fs.GetConfig(ctx),
		client: &Mclient,
	}
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	return f, nil

}
func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.Put(ctx, in, src, options...)
}

func (f *Fs) Root() string {
	return f.root
}

func (f *Fs) String() string {
	return "Web3 Storage FS"
}

func (f *Fs) Precision() time.Duration {
	return time.Millisecond
}

func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.None)
}

func (f *Fs) Features() *fs.Features {
	return f.features
}

func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {

	items, e := (*f.client).List(ctx)
	if e != nil {
		return nil, e
	}
	for _, v := range items {
		//fmt.Printf("", v)

		t, e := time.Parse("YYYY-MM-DDTHH:MM:SSZ", v.Created)
		if e != nil {
			t = time.Now()
		}
		file := Object{
			fs:      f,
			remote:  v.Cid,
			size:    int64(v.DagSize),
			cid:     v.Cid,
			name:    v.Name,
			modTime: t,
		}

		entries = append(entries, file)
	}
	return entries, nil
	//panic("implement me")
}

func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {

	c, e := (*f.client).PutRclone(ctx, in, src, putConfig{dirname: ""})
	if e != nil {
		return nil, e
	}
	return Object{fs: f, cid: c.String(), remote: src.Remote(), size: src.Size()}, nil

}

func (f *Fs) Mkdir(ctx context.Context, dir string) error {

	panic("implement me")
}

func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	panic("implement me")
}

func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {

	c, e := cid.Decode(remote)
	if e != nil {
		return nil, e
	}
	s, e := (*f.client).StatusIpfs(ctx, c)
	if e != nil {
		return nil, fs.ErrorObjectNotFound
	}

	return Object{
		fs:      f,
		remote:  remote,
		size:    int64(s.Links[0].Size),
		modTime: time.Now(),
		name:    s.Links[0].Name,
		cid:     s.Links[0].Cid.String(),
	}, nil

}

// Object is a remote object that has been stat'd (so it exists, but is not necessarily open for reading)
type Object struct {
	fs      *Fs
	remote  string
	size    int64
	modTime time.Time
	name    string
	cid     string

	//contentType string
}

func (o Object) ID() string {
	return o.cid
}

func (o Object) MimeType(ctx context.Context) string {
	return ""
}

func (o Object) String() string {
	return o.name
}

func (o Object) Remote() string {
	return o.remote
}

func (o Object) ModTime(ctx context.Context) time.Time {
	return o.modTime
}

func (o Object) Size() int64 {
	return o.size
}

func (o Object) Fs() fs.Info {
	return o.fs
}

func (o Object) Hash(ctx context.Context, ty hash.Type) (string, error) {
	panic("implement me")
}

func (o Object) Storable() bool {
	return false
}

func (o Object) SetModTime(ctx context.Context, t time.Time) error {
	o.modTime = t
	return nil
}

func (o Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	//o.cid
	cid, _ := cid.Decode(o.cid)
	resp, e := (*o.fs.client).GetIpfsFile(ctx, cid)
	if e != nil {
		return nil, e
	}
	//b, _ := ioutil.ReadAll(resp.Body)
	//println(string(b))

	//defer resp.Body.Close()
	return resp.Body, nil

	//c, e := cid.Decode(o.remote)
	//if e != nil {
	//	return nil, e
	//}
	//resp, error := (*o.fs.client).GetUsingIpfs(ctx, c)
	//if error != nil {
	//	return nil, error
	//}
	//
	//f, e := resp.GetFiles()
	//
	//if e == nil {
	//	fi, e := f.Stat()
	//	if e == nil {
	//		o.size = fi.Size()
	//	}
	//}
	//return f, e

	////println(resp.ContentLength)
	////b, _ := io.ReadAll(resp.Body)
	////
	////println(string(b))
	//
	//cr, e := car.NewCarReader(resp.Body)
	//if e != nil {
	//	return nil, e
	//}
	//println(cr.Header.Roots)
	//bk, be := cr.Next()
	//for be == nil {
	//	println(string(bk.RawData()))
	//
	//	bk, be = cr.Next()
	//}
	//
	//return resp.Body, error
	//(*o.fs.client).Get(ctx,nil)
}

func (o Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	//panic("implement me")
	o.remote = src.Remote()
	o.size = src.Size()

	no, e := o.fs.Put(ctx, in, src, options...)
	if e != nil {
		return e
	}
	o.size = no.Size()

	return nil

}

func (o Object) Remove(ctx context.Context) error {
	println("W3S Don't support remove")
	//return   ("W3S Don't support remove")
	return nil
}

// Name returns the configured name of the file system
func (f *Fs) Name() string {
	return f.name
}

var (
	_ fs.Fs          = &Fs{}
	_ fs.PutStreamer = &Fs{}

	_ fs.Object = &Object{}

	_ fs.MimeTyper = &Object{}
	_ fs.IDer      = &Object{}
)
