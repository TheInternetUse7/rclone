// Package buzzheavier provides an interface to the BuzzHeavier file hosting service.
//
// Authenticated use (account_id set):
//   - Full filesystem: ls, copy, move, mkdir, rmdir, link
//   - rclone copy localfile buzzheavier:myfolder/file.mp4
//   - rclone ls buzzheavier:
//   - rclone link buzzheavier:myfolder/file.mp4
//
// Anonymous use (no account_id):
//   - Upload only; listing and linking by path are not possible
//   - rclone copyto localfile buzzheavier:file.mp4
//   - The public link is printed to the log (use -v to see it)
//   - Save that link — it is the only way to retrieve the file later
package buzzheavier

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
)

const (
	apiBaseURL    = "https://buzzheavier.com/api"
	uploadBaseURL = "https://w.buzzheavier.com"
	downloadBase  = "https://buzzheavier.com"
	minSleep      = pacer.MinSleep(10 * time.Millisecond)
	maxSleep      = pacer.MaxSleep(2 * time.Second)
	decayConstant = pacer.DecayConstant(2)
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "buzzheavier",
		Description: "BuzzHeavier",
		NewFs:       NewFs,
		Options: []fs.Option{
			{
				Name: "account_id",
				Help: "Account ID (Bearer token) for authenticated access.\n" +
					"Found on your BuzzHeavier account page.\n" +
					"Without this, only anonymous uploads are supported.\n" +
					"Anonymous uploads cannot be listed or managed later.",
				Sensitive: true,
			},
			{
				Name: "location_id",
				Help: "Storage location ID for uploads (optional).\n" +
					"Leave blank to use the server-chosen default.\n" +
					"List available locations: https://buzzheavier.com/api/locations",
				Default: "",
			},
		},
	})
}

// Options defines the configuration for this backend
type Options struct {
	AccountID  string `config:"account_id"`
	LocationID string `config:"location_id"`
}

// Fs represents a BuzzHeavier remote
type Fs struct {
	name     string
	root     string
	opt      Options
	features *fs.Features
	srv      *rest.Client // management API (buzzheavier.com/api/*)
	pacer    *fs.Pacer
	rootID   string // directory ID of f.root (authenticated only)
}

// Object describes a BuzzHeavier file
type Object struct {
	fs       *Fs
	remote   string
	id       string // BuzzHeavier file ID — also the URL slug
	size     int64
	modTime  time.Time
	mimeType string
}

// publicURL returns the shareable download URL for this object.
func (o *Object) publicURL() string {
	return downloadBase + "/" + o.id
}

// NewFs constructs an Fs from the path, container:path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}

	srv := rest.NewClient(fshttp.NewClient(ctx)).SetRoot(apiBaseURL)
	if opt.AccountID != "" {
		srv.SetHeader("Authorization", "Bearer "+opt.AccountID)
	}

	f := &Fs{
		name:  name,
		root:  strings.Trim(root, "/"),
		opt:   *opt,
		srv:   srv,
		pacer: fs.NewPacer(ctx, pacer.NewDefault(minSleep, maxSleep, decayConstant)),
	}
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	if opt.AccountID != "" {
		// Resolve (and create if needed) the root directory, cache its ID.
		rootID, err := f.findOrCreateDir(ctx, f.root)
		if err != nil {
			if err == fs.ErrorIsFile {
				f.root = path.Dir(f.root)
				if f.root == "." || f.root == "/" {
					f.root = ""
				}
				// Retry with the parent directory
				rootID, err = f.findOrCreateDir(ctx, f.root)
				if err != nil {
					return nil, err
				}
				f.rootID = rootID
				return f, fs.ErrorIsFile
			}
			return nil, fmt.Errorf("failed to resolve root directory %q: %w", f.root, err)
		}
		f.rootID = rootID
	}

	return f, nil
}

// --- fs.Fs interface ---

func (f *Fs) Name() string             { return f.name }
func (f *Fs) Root() string             { return f.root }
func (f *Fs) Precision() time.Duration { return time.Second }
func (f *Fs) Hashes() hash.Set         { return hash.Set(hash.None) }
func (f *Fs) Features() *fs.Features   { return f.features }
func (f *Fs) String() string           { return fmt.Sprintf("buzzheavier root '%s'", f.root) }

// List the objects and directories in dir into entries.
//
// Requires authentication. Without account_id this always returns an error
// because BuzzHeavier has no anonymous listing API.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	if f.opt.AccountID == "" {
		return nil, fmt.Errorf("buzzheavier: listing requires an account_id — " +
			"anonymous uploads cannot be listed; save the link printed during upload")
	}

	dirID, err := f.dirIDForPath(ctx, dir)
	if err != nil {
		return nil, fs.ErrorDirNotFound
	}

	items, err := f.listDir(ctx, dirID)
	if err != nil {
		return nil, err
	}

	for _, item := range items {
		remote := path.Join(dir, item.Name)
		if item.IsDirectory {
			entries = append(entries, fs.NewDir(remote, item.CreatedAt))
		} else {
			entries = append(entries, &Object{
				fs:      f,
				remote:  remote,
				id:      item.ID,
				size:    item.Size,
				modTime: item.CreatedAt,
			})
		}
	}
	return entries, nil
}

// NewObject finds the Object at remote.
//
// Requires authentication; returns a clear error when used anonymously.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	if f.opt.AccountID == "" {
		// Without authentication we cannot check whether the file exists,
		// so tell rclone "not found" so it proceeds to upload via Put().
		fs.Debugf(f, "NewObject(%q): no account_id, returning ObjectNotFound", remote)
		return nil, fs.ErrorObjectNotFound
	}

	dir := path.Dir(remote)
	if dir == "." {
		dir = ""
	}
	name := path.Base(remote)

	dirID, err := f.dirIDForPath(ctx, dir)
	if err != nil {
		return nil, fs.ErrorObjectNotFound
	}

	items, err := f.listDir(ctx, dirID)
	if err != nil {
		return nil, fs.ErrorObjectNotFound
	}
	for _, item := range items {
		if item.Name == name && !item.IsDirectory {
			return &Object{
				fs:      f,
				remote:  remote,
				id:      item.ID,
				size:    item.Size,
				modTime: item.CreatedAt,
			}, nil
		}
	}
	return nil, fs.ErrorObjectNotFound
}

// Put uploads a file to BuzzHeavier.
//
// Anonymous uploads: the public link is always printed via fs.Infof.
// Run rclone with -v (verbose) to see it. Save it — there is no other way
// to retrieve the file later without an account.
//
// Authenticated uploads: the file is stored in the remote's directory tree
// and can be found later with `rclone ls` or retrieved with `rclone link`.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	dir := path.Dir(remote)
	if dir == "." {
		dir = ""
	}
	name := path.Base(remote)

	var uploadURL string
	if f.opt.AccountID != "" {
		// Authenticated: upload into the correct folder so the file is
		// visible in the user's file manager and can be found by `rclone ls`.
		parentID, err := f.findOrCreateDir(ctx, path.Join(f.root, dir))
		if err != nil {
			return nil, fmt.Errorf("buzzheavier: failed to create parent directory: %w", err)
		}
		uploadURL = uploadBaseURL + "/" + parentID + "/" + name
	} else {
		// Anonymous: flat upload to the root upload endpoint.
		// The location ID can still be chosen to pick a server region.
		uploadURL = uploadBaseURL + "/" + name
		if f.opt.LocationID != "" {
			uploadURL += "?locationId=" + f.opt.LocationID
		}
	}

	fileID, err := f.uploadFile(ctx, uploadURL, in, src.Size())
	if err != nil {
		return nil, err
	}

	obj := &Object{
		fs:      f,
		remote:  remote,
		id:      fileID,
		size:    src.Size(),
		modTime: src.ModTime(ctx),
	}

	// Always surface the public link at NOTICE level.
	// For anonymous users this is the ONLY record of where the file lives.
	fs.Logf(obj, "Uploaded successfully — public link: %s", obj.publicURL())

	return obj, nil
}

// PublicLink returns the public download URL for a remote path.
//
// Usage: rclone link buzzheavier:folder/file.mp4
//
// Requires authentication because there is no API to resolve a path to a
// file ID without an account. For anonymous files, the link was printed
// when the file was uploaded.
func (f *Fs) PublicLink(ctx context.Context, remote string, expire fs.Duration, unlink bool) (string, error) {
	if f.opt.AccountID == "" {
		return "", fmt.Errorf("buzzheavier: link requires account_id — " +
			"the link was printed when the file was uploaded")
	}
	o, err := f.NewObject(ctx, remote)
	if err != nil {
		return "", err
	}
	return o.(*Object).publicURL(), nil
}

// Mkdir creates the directory if it doesn't exist. Requires authentication.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	if f.opt.AccountID == "" {
		return fmt.Errorf("buzzheavier: mkdir requires account_id")
	}
	_, err := f.findOrCreateDir(ctx, path.Join(f.root, dir))
	return err
}

// Rmdir deletes the directory. Requires authentication.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	if f.opt.AccountID == "" {
		return fmt.Errorf("buzzheavier: rmdir requires account_id")
	}
	dirID, err := f.dirIDForPath(ctx, dir)
	if err != nil {
		return fs.ErrorDirNotFound
	}
	return f.deleteItem(ctx, dirID)
}

// --- fs.Object interface ---

func (o *Object) Fs() fs.Info    { return o.fs }
func (o *Object) Remote() string { return o.remote }
func (o *Object) String() string { return o.remote }
func (o *Object) Size() int64    { return o.size }
func (o *Object) Storable() bool { return true }

func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}

func (o *Object) ModTime(ctx context.Context) time.Time { return o.modTime }

func (o *Object) SetModTime(ctx context.Context, t time.Time) error {
	return fs.ErrorCantSetModTime
}

// Open downloads the object. The public URL is used so this works for any
// file regardless of whether the remote is authenticated.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", o.publicURL(), nil)
	if err != nil {
		return nil, err
	}
	fs.OpenOptionAddHTTPHeaders(req.Header, options)

	resp, err := fshttp.NewClient(ctx).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("buzzheavier: download failed: %s", resp.Status)
	}
	return resp.Body, nil
}

// Update replaces the object's content with a new upload.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	_ = o.Remove(ctx) // best-effort delete of old file
	newObj, err := o.fs.Put(ctx, in, src, options...)
	if err != nil {
		return err
	}
	updated := newObj.(*Object)
	o.id = updated.id
	o.size = updated.size
	o.modTime = updated.modTime
	return nil
}

// Remove deletes the object. Requires authentication.
func (o *Object) Remove(ctx context.Context) error {
	if o.fs.opt.AccountID == "" {
		return fmt.Errorf("buzzheavier: remove requires account_id")
	}
	return o.fs.deleteItem(ctx, o.id)
}

// Verify interfaces are fully satisfied at compile time.
var (
	_ fs.Fs           = (*Fs)(nil)
	_ fs.Object       = (*Object)(nil)
	_ fs.PublicLinker = (*Fs)(nil)
)
