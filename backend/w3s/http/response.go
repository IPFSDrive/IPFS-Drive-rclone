package http

import (
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"net/http"

	"github.com/ipfs/go-blockservice"
	"github.com/ipld/go-car"
	"github.com/rclone/rclone/backend/w3s/fs/adapter"
)

// Web3Response is a response to a call to the Get method.
type Web3Response struct {
	*http.Response
	bsvc blockservice.BlockService
}

func NewWeb3Response(r *http.Response, bsvc blockservice.BlockService) *Web3Response {
	return &Web3Response{r, bsvc}
}

func (r *Web3Response) GetFiles() (fs.File, error) {

	f, fsys, err := r.Files()
	if err != nil {
		panic(err)
	}

	info, err := f.Stat()
	if err != nil {
		panic(err)
	}

	if info.IsDir() {

		var pa string
		err = fs.WalkDir(fsys, "/", func(path string, d fs.DirEntry, err error) error {
			info, _ := d.Info()
			fmt.Printf("%s (%d bytes),%b \n", path, info.Size(), info.IsDir())
			if !info.IsDir() {
				pa = path
				return nil
			}

			return err
		})

		if err != nil {
			panic(err)
		}
		println(pa)

		f, e := fsys.Open(pa)
		if e == nil {

			b, e := ioutil.ReadAll(f)
			if e != nil {

				panic(e)
			}
			println(string(b))
		}
		return f, e

	} else {

		fmt.Printf("%s (%d bytes)\n", "cid.String()", info.Size())
		panic("should not goto here")
	}
}

// Files consumes the HTTP response and returns the root file (which may be a
// directory). You can use the returned FileSystem implementation to read
// nested files and directories if the returned file is a directory.
func (r *Web3Response) Files() (fs.File, fs.FS, error) {
	//b, e := ioutil.ReadAll(r.Body)
	//if e != nil {
	//
	//	panic(e)
	//}
	//println(string(b))
	cr, err := car.NewCarReader(r.Body)
	if err != nil {

		return nil, nil, err
	}

	for {
		b, err := cr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, nil, err
		}
		err = r.bsvc.AddBlock(b)
		if err != nil {
			return nil, nil, err
		}
	}

	ctx := r.Request.Context()
	rootCid := cr.Header.Roots[0]

	fs, err := adapter.NewFsWithContext(ctx, rootCid, r.bsvc)
	if err != nil {
		return nil, nil, err
	}

	f, err := fs.Open("/")
	if err != nil {
		return nil, nil, err
	}

	return f, fs, nil
}
