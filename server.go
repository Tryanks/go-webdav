package webdav

import (
	"context"
	"encoding/xml"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-webdav/internal"
	"github.com/gofiber/fiber/v2"
)

const TimeFormat string = "Mon, 02 Jan 2006 15:04:05 GMT"

// bodyStreamWrapper wraps an io.Reader to implement io.ReadCloser
type bodyStreamWrapper struct {
	io.Reader
}

func (b *bodyStreamWrapper) Close() error {
	// Fiber's body stream doesn't need explicit closing
	return nil
}

// FileSystem is a WebDAV server backend.
type FileSystem interface {
	Open(ctx context.Context, name string) (io.ReadCloser, error)
	Stat(ctx context.Context, name string) (*FileInfo, error)
	ReadDir(ctx context.Context, name string, recursive bool) ([]FileInfo, error)
	Create(ctx context.Context, name string, body io.ReadCloser, opts *CreateOptions) (fileInfo *FileInfo, created bool, err error)
	RemoveAll(ctx context.Context, name string, opts *RemoveAllOptions) error
	Mkdir(ctx context.Context, name string) error
	Copy(ctx context.Context, name, dest string, options *CopyOptions) (created bool, err error)
	Move(ctx context.Context, name, dest string, options *MoveOptions) (created bool, err error)
}

// LockSystem provides an interface for lock management implementations.
// This allows users to implement their own lock storage
type LockSystem interface {
	// Lock attempts to create a new lock for the given path with specified parameters.
	// Returns the lock information, whether it was created, and any error.
	Lock(path string, depth internal.Depth, timeout time.Duration, refreshToken string) (lock *internal.Lock, created bool, err error)

	// Unlock removes the lock identified by the given token.
	Unlock(tokenHref string) error

	// HasConflictingLock checks if there are conflicting locks for a path.
	HasConflictingLock(path string, depth internal.Depth, excludeToken string) bool
}

// getDecodedPath returns the URL-decoded path from the Fiber context
func getDecodedPath(c *fiber.Ctx) string {
	path := c.Path()
	decoded, err := url.PathUnescape(path)
	if err != nil {
		// If decoding fails, return the original path
		return path
	}
	return decoded
}

// Handler handles WebDAV HTTP requests. It can be used to create a WebDAV
// server.
type Handler struct {
	FileSystem FileSystem
	LockSystem LockSystem
}

// Handle implements fiber handler.
func (h *Handler) Handle(c *fiber.Ctx) error {
	if h.FileSystem == nil {
		return c.Status(fiber.StatusInternalServerError).SendString("webdav: no filesystem available")
	}

	b := backend{h.FileSystem, h.LockSystem}
	hh := internal.Handler{Backend: &b}
	return hh.Handle(c)
}

// NewHTTPError creates a new error that is associated with an HTTP status code
// and optionally an error that lead to it. Backends can use this functions to
// return errors that convey some semantics (e.g. 404 not found, 403 access
// denied, etc.) while also providing an (optional) arbitrary error context
// (intended for humans).
func NewHTTPError(statusCode int, cause error) error {
	return &internal.HTTPError{Code: statusCode, Err: cause}
}

type backend struct {
	FileSystem FileSystem
	LockSystem LockSystem
}

func (b *backend) Options(c *fiber.Ctx) (caps []string, allow []string, err error) {
	caps = []string{"2"}

	fi, err := b.FileSystem.Stat(c.Context(), getDecodedPath(c))
	if internal.IsNotFound(err) {
		// For non-existent resources, allow creation methods
		return caps, []string{
			fiber.MethodOptions,
			fiber.MethodPut,
			"MKCOL",
			"PROPFIND",
		}, nil
	} else if err != nil {
		return nil, nil, err
	}

	// Base methods supported for all resources
	allow = []string{
		fiber.MethodOptions,
		fiber.MethodDelete,
		"PROPFIND",
		"PROPPATCH",
		"COPY",
		"MOVE",
		"LOCK",
		"UNLOCK",
	}

	if !fi.IsDir {
		// File-specific methods
		allow = append(allow, fiber.MethodHead, fiber.MethodGet, fiber.MethodPut)
	} else {
		// Directory-specific methods
		allow = append(allow, "MKCOL")
	}

	return caps, allow, nil
}

func (b *backend) HeadGet(c *fiber.Ctx) error {
	fi, err := b.FileSystem.Stat(c.Context(), getDecodedPath(c))
	if err != nil {
		return err
	}
	if fi.IsDir {
		return &internal.HTTPError{Code: fiber.StatusMethodNotAllowed}
	}

	f, err := b.FileSystem.Open(c.Context(), getDecodedPath(c))
	if err != nil {
		return err
	}
	defer f.Close()

	c.Set("Content-Length", strconv.FormatInt(fi.Size, 10))
	if fi.MIMEType != "" {
		c.Set("Content-Type", fi.MIMEType)
	}
	if !fi.ModTime.IsZero() {
		c.Set("Last-Modified", fi.ModTime.UTC().Format(TimeFormat))
	}
	if fi.ETag != "" {
		c.Set("ETag", internal.ETag(fi.ETag).String())
	}

	if rs, ok := f.(io.ReadSeeker); ok {
		// For io.ReadSeeker, we need to handle ranges manually in Fiber
		// For now, just copy the content
		if c.Method() != fiber.MethodHead {
			_, err = io.Copy(c.Response().BodyWriter(), rs)
			return err
		}
	} else {
		if c.Method() != fiber.MethodHead {
			_, err = io.Copy(c.Response().BodyWriter(), f)
			return err
		}
	}
	return nil
}

func (b *backend) PropFind(c *fiber.Ctx, propfind *internal.PropFind, depth internal.Depth) (*internal.MultiStatus, error) {
	// TODO: use partial error Response on error

	fi, err := b.FileSystem.Stat(c.Context(), getDecodedPath(c))
	if err != nil {
		return nil, err
	}

	var resps []internal.Response
	if depth != internal.DepthZero && fi.IsDir {
		children, err := b.FileSystem.ReadDir(c.Context(), getDecodedPath(c), depth == internal.DepthInfinity)
		if err != nil {
			return nil, err
		}

		resps = make([]internal.Response, len(children))
		for i, child := range children {
			resp, err := b.propFindFile(propfind, &child)
			if err != nil {
				return nil, err
			}
			resps[i] = *resp
		}
	} else {
		resp, err := b.propFindFile(propfind, fi)
		if err != nil {
			return nil, err
		}

		resps = []internal.Response{*resp}
	}

	return internal.NewMultiStatus(resps...), nil
}

func (b *backend) propFindFile(propfind *internal.PropFind, fi *FileInfo) (*internal.Response, error) {
	props := make(map[xml.Name]internal.PropFindFunc)

	props[internal.ResourceTypeName] = func(*internal.RawXMLValue) (interface{}, error) {
		var types []xml.Name
		if fi.IsDir {
			types = append(types, internal.CollectionName)
		}
		return internal.NewResourceType(types...), nil
	}

	props[internal.SupportedLockName] = internal.PropFindValue(&internal.SupportedLock{
		LockEntries: []internal.LockEntry{{
			LockScope: internal.LockScope{Exclusive: &struct{}{}},
			LockType:  internal.LockType{Write: &struct{}{}},
		}},
	})

	if !fi.IsDir {
		props[internal.GetContentLengthName] = internal.PropFindValue(&internal.GetContentLength{
			Length: fi.Size,
		})

		if !fi.ModTime.IsZero() {
			props[internal.GetLastModifiedName] = internal.PropFindValue(&internal.GetLastModified{
				LastModified: internal.Time(fi.ModTime),
			})
		}

		if fi.MIMEType != "" {
			props[internal.GetContentTypeName] = internal.PropFindValue(&internal.GetContentType{
				Type: fi.MIMEType,
			})
		}

		if fi.ETag != "" {
			props[internal.GetETagName] = internal.PropFindValue(&internal.GetETag{
				ETag: internal.ETag(fi.ETag),
			})
		}
	}

	return internal.NewPropFindResponse(fi.Path, propfind, props)
}

func (b *backend) PropPatch(c *fiber.Ctx, update *internal.PropertyUpdate) (*internal.Response, error) {
	fi, err := b.FileSystem.Stat(c.Context(), getDecodedPath(c))
	if err != nil {
		return nil, err
	}

	resp := &internal.Response{Hrefs: []internal.Href{internal.Href{Path: fi.Path}}}

	// Define modifiable properties according to RFC 4918
	modifiableProps := map[xml.Name]bool{
		{Space: "DAV:", Local: "displayname"}: true,
		// Add other modifiable properties as needed
	}

	// Define protected properties that cannot be modified
	protectedProps := map[xml.Name]bool{
		internal.GetContentLengthName: true,
		internal.GetLastModifiedName:  true,
		internal.GetETagName:          true,
		internal.GetContentTypeName:   true,
		internal.ResourceTypeName:     true,
		internal.SupportedLockName:    true,
	}

	for _, set := range update.Set {
		for _, raw := range set.Prop.Raw {
			xmlName, ok := raw.XMLName()
			if !ok {
				continue
			}

			emptyVal := internal.NewRawXMLElement(xmlName, nil, nil)

			if protectedProps[xmlName] {
				// Protected property - cannot be modified
				if err := resp.EncodeProp(fiber.StatusForbidden, emptyVal); err != nil {
					return nil, err
				}
			} else if modifiableProps[xmlName] {
				// Modifiable property - allow modification
				if err := resp.EncodeProp(fiber.StatusOK, emptyVal); err != nil {
					return nil, err
				}
			} else {
				// Unknown property - treat as not found
				if err := resp.EncodeProp(fiber.StatusNotFound, emptyVal); err != nil {
					return nil, err
				}
			}
		}
	}

	for _, remove := range update.Remove {
		for _, raw := range remove.Prop.Raw {
			xmlName, ok := raw.XMLName()
			if !ok {
				continue
			}

			emptyVal := internal.NewRawXMLElement(xmlName, nil, nil)

			if protectedProps[xmlName] {
				// Protected property - cannot be removed
				if err := resp.EncodeProp(fiber.StatusForbidden, emptyVal); err != nil {
					return nil, err
				}
			} else if modifiableProps[xmlName] {
				// Modifiable property - allow removal
				if err := resp.EncodeProp(fiber.StatusOK, emptyVal); err != nil {
					return nil, err
				}
			} else {
				// Unknown property - treat as not found
				if err := resp.EncodeProp(fiber.StatusNotFound, emptyVal); err != nil {
					return nil, err
				}
			}
		}
	}

	if len(resp.PropStats) == 0 {
		return nil, internal.HTTPErrorf(fiber.StatusBadRequest,
			"webdav: request missing properties to update")
	}

	return resp, nil
}

func (b *backend) Put(c *fiber.Ctx) error {
	ifNoneMatch := ConditionalMatch(c.Get("If-None-Match"))
	ifMatch := ConditionalMatch(c.Get("If-Match"))

	opts := CreateOptions{
		IfNoneMatch: ifNoneMatch,
		IfMatch:     ifMatch,
	}

	// Handle potential nil body stream
	var body io.ReadCloser
	bodyStream := c.Request().BodyStream()
	if bodyStream != nil {
		body = &bodyStreamWrapper{bodyStream}
	} else {
		body = &bodyStreamWrapper{strings.NewReader("")}
	}

	fi, created, err := b.FileSystem.Create(c.Context(), getDecodedPath(c), body, &opts)
	if err != nil {
		return err
	}

	if fi.MIMEType != "" {
		c.Set("Content-Type", fi.MIMEType)
	}
	if !fi.ModTime.IsZero() {
		c.Set("Last-Modified", fi.ModTime.UTC().Format(TimeFormat))
	}
	if fi.ETag != "" {
		c.Set("ETag", internal.ETag(fi.ETag).String())
	}

	if created {
		c.Status(fiber.StatusCreated)
	} else {
		c.Status(fiber.StatusNoContent)
	}

	return nil
}

func (b *backend) Delete(c *fiber.Ctx) error {
	ifNoneMatch := ConditionalMatch(c.Get("If-None-Match"))
	ifMatch := ConditionalMatch(c.Get("If-Match"))

	opts := RemoveAllOptions{
		IfNoneMatch: ifNoneMatch,
		IfMatch:     ifMatch,
	}
	return b.FileSystem.RemoveAll(c.Context(), getDecodedPath(c), &opts)
}

func (b *backend) Mkcol(c *fiber.Ctx) error {
	if c.Get("Content-Type") != "" {
		return internal.HTTPErrorf(fiber.StatusUnsupportedMediaType, "webdav: request body not supported in MKCOL request")
	}
	err := b.FileSystem.Mkdir(c.Context(), getDecodedPath(c))
	if internal.IsNotFound(err) {
		return &internal.HTTPError{Code: fiber.StatusConflict, Err: err}
	}
	return err
}

func (b *backend) Copy(c *fiber.Ctx, dest *internal.Href, recursive, overwrite bool) (created bool, err error) {
	options := CopyOptions{
		NoRecursive: !recursive,
		NoOverwrite: !overwrite,
	}
	created, err = b.FileSystem.Copy(c.Context(), getDecodedPath(c), dest.Path, &options)
	if os.IsExist(err) {
		return false, &internal.HTTPError{fiber.StatusPreconditionFailed, err}
	}
	return created, err
}

func (b *backend) Move(c *fiber.Ctx, dest *internal.Href, overwrite bool) (created bool, err error) {
	options := MoveOptions{
		NoOverwrite: !overwrite,
	}
	created, err = b.FileSystem.Move(c.Context(), getDecodedPath(c), dest.Path, &options)
	if os.IsExist(err) {
		return false, &internal.HTTPError{fiber.StatusPreconditionFailed, err}
	}
	return created, err
}

func (b *backend) Lock(c *fiber.Ctx, depth internal.Depth, timeout time.Duration, refreshToken string) (lock *internal.Lock, created bool, err error) {
	return b.LockSystem.Lock(getDecodedPath(c), depth, timeout, refreshToken)
}

func (b *backend) Unlock(c *fiber.Ctx, tokenHref string) error {
	return b.LockSystem.Unlock(tokenHref)
}

// BackendSuppliedHomeSet represents either a CalDAV calendar-home-set or a
// CardDAV addressbook-home-set. It should only be created via
// caldav.NewCalendarHomeSet or carddav.NewAddressBookHomeSet. Only to
// be used server-side, for listing a user's home sets as determined by the
// (external) backend.
type BackendSuppliedHomeSet interface {
	GetXMLName() xml.Name
}

// UserPrincipalBackend can determine the current user's principal URL for a
// given request context.
type UserPrincipalBackend interface {
	CurrentUserPrincipal(ctx context.Context) (string, error)
}

// Capability indicates the features that a server supports.
type Capability string

// ServePrincipalOptions holds options for ServePrincipal.
type ServePrincipalOptions struct {
	CurrentUserPrincipalPath string
	HomeSets                 []BackendSuppliedHomeSet
	Capabilities             []Capability
}

// ServePrincipal replies to requests for a principal URL.
func ServePrincipal(c *fiber.Ctx, options *ServePrincipalOptions) error {
	switch c.Method() {
	case fiber.MethodOptions:
		caps := []string{"1", "3"}
		for _, cap := range options.Capabilities {
			caps = append(caps, string(cap))
		}
		allow := []string{fiber.MethodOptions, "PROPFIND", "REPORT", "DELETE", "MKCOL"}
		c.Set("DAV", strings.Join(caps, ", "))
		c.Set("Allow", strings.Join(allow, ", "))
		c.Status(fiber.StatusNoContent)
		return nil
	case "PROPFIND":
		if err := servePrincipalPropfind(c, options); err != nil {
			internal.ServeError(c, err)
			return err
		}
		return nil
	default:
		return c.Status(fiber.StatusMethodNotAllowed).SendString("unsupported method")
	}
}

func servePrincipalPropfind(c *fiber.Ctx, options *ServePrincipalOptions) error {
	var propfind internal.PropFind
	if err := internal.DecodeXMLRequest(c, &propfind); err != nil {
		return err
	}
	props := map[xml.Name]internal.PropFindFunc{
		internal.ResourceTypeName: func(*internal.RawXMLValue) (interface{}, error) {
			return internal.NewResourceType(principalName), nil
		},
		internal.CurrentUserPrincipalName: func(*internal.RawXMLValue) (interface{}, error) {
			return &internal.CurrentUserPrincipal{Href: internal.Href{Path: options.CurrentUserPrincipalPath}}, nil
		},
	}

	// TODO: handle Depth and more properties

	for _, homeSet := range options.HomeSets {
		hs := homeSet // capture variable for closure
		props[homeSet.GetXMLName()] = func(*internal.RawXMLValue) (interface{}, error) {
			return hs, nil
		}
	}

	resp, err := internal.NewPropFindResponse(getDecodedPath(c), &propfind, props)
	if err != nil {
		return err
	}

	ms := internal.NewMultiStatus(*resp)
	return internal.ServeMultiStatus(c, ms)
}
