package ipfs

import (
	"context"
	"fmt"
	"github.com/pkg/errors"
	"github.com/rclone/rclone/backend/ipfs/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"io"
	"path"
	"strings"
	"sync"
	"time"
)

// TODO: Use "cat" to get file size rather than converting the cumulative file size
// TODO: add new hash type (compute local IPFS hashes)
// TODO: write documentation for IPFS backend

// Register with Fs
func init() {
	fsi := &fs.RegInfo{
		Name:        "ipfs",
		Description: "IPFS",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:     "url",
			Help:     "IPFS API server URL.",
			Required: true,
			Default:  LocalGateway,
			Examples: []fs.OptionExample{{
				Value: LocalGateway,
				Help:  "Connect to your local IPFS API server",
			}, {
				Value: PublicGateway,
				Help:  "Connect to the public IPFS gateway (read only!)",
			}},
		}, {
			Name: "root",
			Help: "IPFS root ref path.\n" +
				"Leave it empty to use IPFS MFS.\n" +
				"Otherwise, set it to IPFS path (format \"/ipfs/<HASH>\") or IPNS path (format \"/ipns/<HASH>\"). ",
			Default: "",
		}, {
			Name:     "update_period",
			Help:     "Time between periodic update to persist modifications (ignore in read only mode).",
			Advanced: true,
			Default:  DefaultUpdatePeriod,
		}},
	}
	fs.Register(fsi)
}

// Options defines the configuration for this backend
type Options struct {
	Endpoint     string      `config:"url"`
	IpfsRoot     string      `config:"root"`
	UpdatePeriod fs.Duration `config:"update_period"`
}

// Max size of a IPFS object data after which the IPFS chunker will
// chunk the original file
const MaxChunkSize = int64(262144)

const LocalGateway = "http://localhost:5001"
const PublicGateway = "https://ipfs.io"

type SharedRoots struct {
	sync.RWMutex

	// IPFS roots indexed by Fs Options
	cache map[Options]*Root
}

var (
	DefaultModTime      = time.Unix(0, 0)
	DefaultUpdatePeriod = fs.Duration(15 * time.Second)
	sharedRoot          = &SharedRoots{cache: make(map[Options]*Root)}
)

// ------------------------------------------------------------

// Fs stores the interface to the remote HTTP files
type Fs struct {
	name          string
	root          string
	features      *fs.Features // optional features
	opt           Options      // options for this backend
	api           *api.Client  // the connection to the server
	ipfsRoot      *Root
	_emptyDirHash string // IPFS hash of an empty dir
}

// Object is a remote object that has been stat'd (so it exists, but is not necessarily open for reading)
type Object struct {
	fs       *Fs
	remote   string
	size     int64
	ipfsHash string
}

// ------------------------------------------------------------

// NewFs creates a new Fs object from the name and root. It connects to
// the host specified in the config file.
func NewFs(ctx context.Context, name string, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	opt.Endpoint = removeTrailingSlash(opt.Endpoint)
	client := fshttp.NewClient(ctx)

	f := &Fs{
		name: name,
		root: root,
		opt:  *opt,
		api:  api.NewApi(client, opt.Endpoint),
	}
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	sharedRoot.Lock()
	ipfsRoot := sharedRoot.cache[*opt]
	if ipfsRoot == nil {
		ipfsRoot, err = NewRoot(ctx, f)
		if err != nil {
			return nil, err
		}
		sharedRoot.cache[*opt] = ipfsRoot
	}
	f.ipfsRoot = ipfsRoot
	sharedRoot.Unlock()

	var fsError error = nil
	if root != "" {
		// Check to see if the root actually an existing file
		remote := path.Base(root)
		f.root = path.Dir(root)
		if f.root == "." {
			f.root = ""
		}

		_, err := f.NewObject(ctx, remote)
		if err != nil {
			if err == fs.ErrorObjectNotFound || errors.Cause(err) == fs.ErrorNotAFile || err == fs.ErrorNotAFile {
				// Remote is file or doesn't exist => reset root
				f.root = root
			} else {
				return nil, err
			}
		} else {
			// return an error with an fs which points to the parent
			fsError = fs.ErrorIsFile
		}
	}

	return f, fsError
}

// Get or fetch the IPFS empty directory hash
func (f *Fs) emptyDirHash(ctx context.Context) (string, error) {
	if f._emptyDirHash == "" {
		result, err := f.api.ObjectNewDir(ctx)
		if err != nil {
			return "", err
		}
		f._emptyDirHash = result.Hash
	}
	return f._emptyDirHash, nil
}

func removeTrailingSlash(s string) string {
	if strings.HasPrefix(s, "/") {
		// Should not start with "/"
		return s[1:]
	}
	return s
}

func (f *Fs) relativePath(remote string) (relativePath string) {
	// Should not start with "/"
	return removeTrailingSlash(path.Join(f.root, remote))
}

// Convert IPFS object cumulative size to actual file size
// Only for small file of size < 262267
func convertSmallFileSize(cumulativeSize int64) int64 {
	switch {
	case cumulativeSize == 0:
		return 0
	case cumulativeSize < 9:
		return cumulativeSize - 6
	case cumulativeSize < 131:
		return cumulativeSize - 8
	case cumulativeSize < 139:
		return cumulativeSize - 9
	case cumulativeSize < 16388:
		return cumulativeSize - 11
	case cumulativeSize < 16398:
		return cumulativeSize - 12
	default:
		return cumulativeSize - 14
	}
}

// Convert IPFS object size to actual file size
func (f *Fs) convertToFileSize(stat api.ObjectStat) int64 {
	// Calculate file size
	var fileSize int64
	cumulativeSize := stat.CumulativeSize
	if cumulativeSize < (MaxChunkSize + 123) {
		// Single chunk file
		fileSize = convertSmallFileSize(cumulativeSize)
	} else {
		// Multiple chunk file
		i := cumulativeSize - stat.BlockSize
		maxSizeChunks := i / (MaxChunkSize + 14)
		remainingSizeChunk := i % (MaxChunkSize + 14)
		fileSize = i - (maxSizeChunks * 14) - (remainingSizeChunk - convertSmallFileSize(remainingSizeChunk))
	}
	return fileSize
}

func (f *Fs) checkReadOnly() error {
	if f.ipfsRoot.isReadOnly {
		return errors.New("IPFS path '" + f.opt.IpfsRoot + "' is read only!")
	}
	return nil
}

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// SharedRoots of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String converts this Fs to a string
func (f *Fs) String() string {
	return fmt.Sprintf("ipfs files root '%s'", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.None)
}

// Precision return the precision of this Fs
func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

// List the objects and directories in dir into entries.  The
// entries can be returned in any order but should be for a
// complete directory.
//
// dir should be "" to list the root, and should not have
// trailing slashes.
//
// This should return ErrorDirNotFound if the directory isn't
// found.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	f.ipfsRoot.RLock()
	rootHash := f.ipfsRoot.hash
	f.ipfsRoot.RUnlock()
	absolutePath := path.Join(rootHash, f.relativePath(dir))
	links, err := f.api.Ls(ctx, absolutePath)
	if err != nil {
		if _, ok := err.(*api.Error); ok {
			return nil, fs.ErrorDirNotFound
		}
		return nil, err
	}

	for _, link := range links {
		remote := path.Join(dir, link.Name)
		if link.Type == api.FileEntryTypeFolder {
			d := fs.NewDir(remote, DefaultModTime)
			entries = append(entries, d)
		} else {
			stat, err := f.api.ObjectStat(ctx, path.Join(rootHash, f.relativePath(remote)))
			if err != nil {
				return nil, err
			}
			o := newObject(f, remote, stat)
			entries = append(entries, o)
		}
	}
	return entries, nil
}

func newObject(f *Fs, remote string, stat *api.ObjectStat) *Object {
	return &Object{
		fs:       f,
		remote:   remote,
		size:     f.convertToFileSize(*stat),
		ipfsHash: stat.Hash,
	}
}

// NewObject finds the Object at remote.  If it can't be found
// it returns the error fs.ErrorObjectNotFound. If is a directory
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	f.ipfsRoot.RLock()
	rootHash := f.ipfsRoot.hash
	f.ipfsRoot.RUnlock()
	absolutePath := path.Join(rootHash, f.relativePath(remote))
	stat, err := f.api.ObjectStat(ctx, absolutePath)
	if err != nil {
		if _, ok := err.(*api.Error); ok {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}

	var isFile bool
	dir, file := path.Split(absolutePath)
	if dir != rootHash {
		links, err := f.api.Ls(ctx, dir)
		if err != nil {
			return nil, err
		}
		for _, link := range links {
			if link.Name == file {
				isFile = link.Type == api.FileEntryTypeFile
				break
			}
		}
	}
	if !isFile {
		return nil, fs.ErrorNotAFile
	}
	o := newObject(f, remote, stat)
	return o, nil
}

// Put the object
//
// Copy the reader in to the new object which is returned
//
// The new object may have been created if an error is returned
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	err := f.checkReadOnly()
	if err != nil {
		return nil, err
	}
	_, file := path.Split(src.Remote())
	fileAdded, err := f.api.Add(ctx, in, file, options...)
	if err != nil {
		return nil, err
	}
	objectPath := f.relativePath(src.Remote())

	f.ipfsRoot.Lock()
	result, err := f.api.ObjectPatchAddLink(ctx, f.ipfsRoot.hash, objectPath, fileAdded.Hash)
	if err != nil {
		f.ipfsRoot.Unlock()
		return nil, err
	}
	f.ipfsRoot.hash = result.Hash
	f.ipfsRoot.Unlock()
	return f.NewObject(ctx, src.Remote())
}

// Mkdir creates the container if it doesn't exist
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	err := f.checkReadOnly()
	if err != nil {
		return err
	}
	emptyDirHash, err := f.emptyDirHash(ctx)
	if err != nil {
		return err
	}

	f.ipfsRoot.Lock()
	defer f.ipfsRoot.Unlock()
	dirPath := f.relativePath(dir)

	_, err = f.api.ObjectStat(ctx, path.Join(f.ipfsRoot.hash, dirPath))
	if err == nil {
		// path already exists
		return nil
	}

	result, err := f.api.ObjectPatchAddLink(ctx, f.ipfsRoot.hash, dirPath, emptyDirHash)
	if err != nil {
		return err
	}
	f.ipfsRoot.hash = result.Hash
	return nil
}

// Rmdir deletes the root folder
//
// Returns ErrorDirectoryNotEmpty if it isn't empty
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	err := f.checkReadOnly()
	if err != nil {
		return err
	}
	f.ipfsRoot.Lock()
	defer f.ipfsRoot.Unlock()
	absolutePath := path.Join(f.ipfsRoot.hash, f.relativePath(dir))
	stat, err := f.api.ObjectStat(ctx, absolutePath)
	if err != nil {
		if _, ok := err.(*api.Error); ok {
			return fs.ErrorDirNotFound
		}
		return err
	}
	// Should not have children
	if stat.NumLinks > 0 {
		return fs.ErrorDirectoryNotEmpty
	}

	dirPath := f.relativePath(dir)
	result, err := f.api.ObjectPatchRmLink(ctx, f.ipfsRoot.hash, dirPath)
	if err != nil {
		return err
	}
	f.ipfsRoot.hash = result.Hash
	return nil
}

func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	err := f.checkReadOnly()
	if err != nil {
		return nil, err
	}
	objectPath := f.relativePath(remote)
	var ipfsObject = src.(*Object)
	f.ipfsRoot.Lock()
	result, err := f.api.ObjectPatchAddLink(ctx, f.ipfsRoot.hash, objectPath, ipfsObject.ipfsHash)
	if err != nil {
		f.ipfsRoot.Unlock()
		return nil, err
	}
	f.ipfsRoot.hash = result.Hash
	f.ipfsRoot.Unlock()
	return f.NewObject(ctx, remote)
}

func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (o fs.Object, err error) {
	err = f.checkReadOnly()
	if err != nil {
		return nil, err
	}
	if o, err = f.Copy(ctx, src, remote); err != nil {
		return nil, err
	}
	if err = src.Remove(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote string, dstRemote string) error {
	err := f.checkReadOnly()
	if err != nil {
		return err
	}
	srcFs := src.(*Fs)
	f.ipfsRoot.Lock()
	defer f.ipfsRoot.Unlock()

	// Check dest dir does not exist
	dstRelativePath := f.relativePath(dstRemote)
	destAbsolutePath := path.Join(f.ipfsRoot.hash, dstRelativePath)
	destStat, err := f.api.ObjectStat(ctx, destAbsolutePath)
	if destStat != nil {
		return fs.ErrorDirExists
	}

	// Fetch source dir stats (for the hash)
	srcRelativePath := srcFs.relativePath(srcRemote)
	srcAbsolutePath := path.Join(f.ipfsRoot.hash, srcRelativePath)
	srcStat, err := f.api.ObjectStat(ctx, srcAbsolutePath)
	if err != nil {
		return err
	}

	// Copy dir by hash
	result, err := f.api.ObjectPatchAddLink(ctx, f.ipfsRoot.hash, dstRelativePath, srcStat.Hash)
	if err != nil {
		return err
	}
	f.ipfsRoot.hash = result.Hash

	// Remove original dir
	result, err = srcFs.api.ObjectPatchRmLink(ctx, f.ipfsRoot.hash, srcRelativePath)
	if err != nil {
		return err
	}
	f.ipfsRoot.hash = result.Hash
	return nil
}

func (f *Fs) MergeDirs(ctx context.Context, dirs []fs.Directory) error {
	err := f.checkReadOnly()
	if err != nil {
		return err
	}
	firstDirectory := dirs[0]
	srcPath := f.relativePath(firstDirectory.Remote())

	f.ipfsRoot.Lock()
	defer f.ipfsRoot.Unlock()
	workingRootHash := f.ipfsRoot.hash
	for _, dir := range dirs[1:] {
		absolutePath := path.Join(workingRootHash, f.root, dir.Remote())
		links, err := f.api.Ls(ctx, absolutePath)
		if err != nil {
			return err
		}

		for _, link := range links {
			relativePath := path.Join(srcPath, link.Name)
			result, err := f.api.ObjectPatchAddLink(ctx, workingRootHash, relativePath, link.Hash)
			if err != nil {
				return err
			}
			workingRootHash = result.Hash
		}

		result, err := f.api.ObjectPatchRmLink(ctx, workingRootHash, f.relativePath(dir.Remote()))
		if err != nil {
			return err
		}
		workingRootHash = result.Hash
	}
	f.ipfsRoot.hash = workingRootHash
	return nil
}

func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.Put(ctx, in, src, options...)
}

func (f *Fs) Purge(ctx context.Context, dir string) error {
	err := f.checkReadOnly()
	if err != nil {
		return err
	}
	f.ipfsRoot.Lock()
	defer f.ipfsRoot.Unlock()
	if f.root == "" {
		// If root folder => replace with empty dir Hash
		emptyDirHash, err := f.emptyDirHash(ctx)
		if err != nil {
			return err
		}
		f.ipfsRoot.hash = emptyDirHash
	} else {
		// Else => remove dir
		result, err := f.api.ObjectPatchRmLink(ctx, f.ipfsRoot.hash, f.root)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				return fs.ErrorDirNotFound
			}
			return err
		}
		f.ipfsRoot.hash = result.Hash
	}
	return nil
}

//ctx context.Context, remote string,
func (f *Fs) PublicLink(ctx context.Context, remote string, expire fs.Duration, unlink bool) (string, error) {
	f.ipfsRoot.RLock()
	ipfsHash := f.ipfsRoot.hash
	ipnsPath := f.ipfsRoot.ipnsPath
	f.ipfsRoot.RUnlock()

	var urlPath string
	if ipnsPath == "" {
		// IPFS path
		urlPath = path.Join("/ipfs", ipfsHash, f.relativePath(remote))
	} else {
		// IPNS path
		urlPath = path.Join(ipnsPath, f.relativePath(remote))
	}

	// Check path exists
	_, err := f.api.ObjectStat(ctx, path.Join(ipfsHash, f.relativePath(remote)))
	if err != nil {
		return "", err
	}
	return PublicGateway + urlPath, nil
}

// ------------------------------------------------------------

// Fs returns the parent Fs
func (o Object) Fs() fs.Info {
	return o.fs
}

func (o Object) relativePath() string {
	return o.fs.relativePath(o.Remote())
}

// Return a string version
func (o Object) String() string {
	//if o == nil {
	//	return "<nil>"
	//}
	return o.remote
}

// Remote returns the remote path
func (o Object) Remote() string {
	return o.remote
}

// Hash returns the SHA-1 of an object returning a lowercase hex string
func (o Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	return o.size
}

// ModTime returns the modification time of the object
func (o Object) ModTime(ctx context.Context) time.Time {
	return DefaultModTime
}

// SetModTime sets the modification time of the local fs object
func (o Object) SetModTime(ctx context.Context, modTime time.Time) error {
	// Addition of modification time on IPFS is discussed at:
	// https://github.com/ipfs/unixfs-v2/issues/1
	return fs.ErrorCantSetModTime
}

// Storable returns a boolean showing whether this object storable
func (o Object) Storable() bool {
	return true
}

// Open an object for read
func (o Object) Open(ctx context.Context, options ...fs.OpenOption) (in io.ReadCloser, err error) {
	o.fs.ipfsRoot.RLock()
	absolutePath := path.Join(o.fs.ipfsRoot.hash, o.relativePath())
	o.fs.ipfsRoot.RUnlock()
	return o.fs.api.Cat(ctx, absolutePath, o.Size(), options...)
}

// Update the object with the contents of the io.Reader, modTime and size
//
// If existing is set then it updates the object rather than creating a new one
//
// The new object may have been created if an error is returned
func (o Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	o2, err := o.fs.Put(ctx, in, src, options...)
	//o2, err := o.fs.Put(ctx, in, o, options...) chy changed
	//
	if err != nil {
		return err
	}

	o.ipfsHash = o2.(*Object).ipfsHash

	o.size = o2.Size()
	return nil
}

// Remove an object
func (o Object) Remove(ctx context.Context) error {
	err := o.fs.checkReadOnly()
	if err != nil {
		return err
	}
	o.fs.ipfsRoot.Lock()
	defer o.fs.ipfsRoot.Unlock()
	result, err := o.fs.api.ObjectPatchRmLink(ctx, o.fs.ipfsRoot.hash, o.relativePath())
	if err != nil {
		if _, ok := err.(*api.Error); ok {
			return fs.ErrorObjectNotFound
		}
		return err
	}
	o.fs.ipfsRoot.hash = result.Hash
	return nil
}

func (o *Object) ID() string {
	return o.ipfsHash
}

// Check the interfaces are satisfied
var (
	_ fs.Fs     = &Fs{}
	_ fs.Object = &Object{}

	// Optional Fs
	_ fs.Copier       = (*Fs)(nil)
	_ fs.Mover        = (*Fs)(nil)
	_ fs.PublicLinker = (*Fs)(nil)
	_ fs.Purger       = (*Fs)(nil)
	_ fs.PutStreamer  = (*Fs)(nil)
	_ fs.MergeDirser  = (*Fs)(nil)
	_ fs.DirMover     = (*Fs)(nil)

	// Optional Object
	_ fs.IDer = &Object{}
)
