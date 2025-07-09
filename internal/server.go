package internal

import (
	"encoding/xml"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

func ServeError(c *fiber.Ctx, err error) {
	code := fiber.StatusInternalServerError
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		code = httpErr.Code
	}

	var errElt *Error
	if errors.As(err, &errElt) {
		c.Status(code)
		ServeXML(c).Encode(errElt)
		return
	}

	c.Status(code).SendString(err.Error())
}

func isContentXML(c *fiber.Ctx) bool {
	t, _, _ := mime.ParseMediaType(c.Get("Content-Type"))
	return t == "application/xml" || t == "text/xml"
}

func ensureRequestBodyEmpty(c *fiber.Ctx) error {
	body := c.Body()
	if len(body) > 0 {
		return HTTPErrorf(fiber.StatusBadRequest, "webdav: unsupported request body")
	}
	return nil
}

func DecodeXMLRequest(c *fiber.Ctx, v interface{}) error {
	if !isContentXML(c) {
		return HTTPErrorf(fiber.StatusBadRequest, "webdav: expected application/xml request")
	}

	body := c.Body()
	if err := xml.NewDecoder(strings.NewReader(string(body))).Decode(v); err != nil {
		return &HTTPError{fiber.StatusBadRequest, err}
	}
	return nil
}

func IsRequestBodyEmpty(c *fiber.Ctx) bool {
	body := c.Body()
	return len(body) == 0
}

func ServeXML(c *fiber.Ctx) *xml.Encoder {
	c.Set("Content-Type", "application/xml; charset=\"utf-8\"")
	c.Write([]byte(xml.Header))
	return xml.NewEncoder(c.Response().BodyWriter())
}

func ServeMultiStatus(c *fiber.Ctx, ms *MultiStatus) error {
	// TODO: streaming
	c.Status(fiber.StatusMultiStatus)
	return ServeXML(c).Encode(ms)
}

type Backend interface {
	Options(c *fiber.Ctx) (caps []string, allow []string, err error)
	HeadGet(c *fiber.Ctx) error
	PropFind(c *fiber.Ctx, pf *PropFind, depth Depth) (*MultiStatus, error)
	PropPatch(c *fiber.Ctx, pu *PropertyUpdate) (*Response, error)
	Put(c *fiber.Ctx) error
	Delete(c *fiber.Ctx) error
	Mkcol(c *fiber.Ctx) error
	Copy(c *fiber.Ctx, dest *Href, recursive, overwrite bool) (created bool, err error)
	Move(c *fiber.Ctx, dest *Href, overwrite bool) (created bool, err error)
	Lock(c *fiber.Ctx, depth Depth, timeout time.Duration, refreshToken string) (lock *Lock, created bool, err error)
	Unlock(c *fiber.Ctx, tokenHref string) error
}

type Lock struct {
	Href    string
	Root    string
	Timeout time.Duration
}

type Handler struct {
	Backend Backend
}

func (h *Handler) Handle(c *fiber.Ctx) error {
	var err error
	if h.Backend == nil {
		err = fmt.Errorf("webdav: no backend available")
	} else {
		switch c.Method() {
		case fiber.MethodOptions:
			err = h.handleOptions(c)
		case fiber.MethodGet, fiber.MethodHead:
			err = h.Backend.HeadGet(c)
		case fiber.MethodPut:
			err = h.Backend.Put(c)
		case fiber.MethodDelete:
			// TODO: send a multistatus in case of partial failure
			err = h.Backend.Delete(c)
			if err == nil {
				c.Status(fiber.StatusNoContent)
			}
		case "PROPFIND":
			err = h.handlePropfind(c)
		case "PROPPATCH":
			err = h.handleProppatch(c)
		case "MKCOL":
			err = h.Backend.Mkcol(c)
			if err == nil {
				c.Status(fiber.StatusCreated)
			}
		case "COPY", "MOVE":
			err = h.handleCopyMove(c)
		case "LOCK":
			err = h.handleLock(c)
		case "UNLOCK":
			err = h.handleUnlock(c)
		default:
			err = HTTPErrorf(fiber.StatusMethodNotAllowed, "webdav: unsupported method")
		}
	}

	if err != nil {
		ServeError(c, err)
		return err
	}
	return nil
}

func (h *Handler) handleOptions(c *fiber.Ctx) error {
	caps, allow, err := h.Backend.Options(c)
	if err != nil {
		return err
	}
	caps = append([]string{"1", "3"}, caps...)

	c.Set("DAV", strings.Join(caps, ", "))
	c.Set("Allow", strings.Join(allow, ", "))
	c.Status(fiber.StatusNoContent)
	return nil
}

func (h *Handler) handlePropfind(c *fiber.Ctx) error {
	var propfind PropFind
	if isContentXML(c) {
		if err := DecodeXMLRequest(c, &propfind); err != nil {
			return err
		}
	} else {
		if err := ensureRequestBodyEmpty(c); err != nil {
			return err
		}
		propfind.AllProp = &struct{}{}
	}

	depth := DepthInfinity
	if s := c.Get("Depth"); s != "" {
		var err error
		depth, err = ParseDepth(s)
		if err != nil {
			return &HTTPError{fiber.StatusBadRequest, err}
		}
	}

	ms, err := h.Backend.PropFind(c, &propfind, depth)
	if err != nil {
		return err
	}

	return ServeMultiStatus(c, ms)
}

type PropFindFunc func(raw *RawXMLValue) (interface{}, error)

func PropFindValue(value interface{}) PropFindFunc {
	return func(raw *RawXMLValue) (interface{}, error) {
		return value, nil
	}
}

func NewPropFindResponse(path string, propfind *PropFind, props map[xml.Name]PropFindFunc) (*Response, error) {
	resp := &Response{Hrefs: []Href{Href{Path: path}}}

	if _, ok := props[ResourceTypeName]; !ok {
		props[ResourceTypeName] = PropFindValue(NewResourceType())
	}

	if propfind.PropName != nil {
		for xmlName, _ := range props {
			emptyVal := NewRawXMLElement(xmlName, nil, nil)
			if err := resp.EncodeProp(http.StatusOK, emptyVal); err != nil {
				return nil, err
			}
		}
	} else if propfind.AllProp != nil {
		// TODO: add support for propfind.Include
		for xmlName, f := range props {
			emptyVal := NewRawXMLElement(xmlName, nil, nil)

			val, err := f(emptyVal)

			code := http.StatusOK
			if err != nil {
				// TODO: don't throw away error message here
				code = HTTPErrorFromError(err).Code
				val = emptyVal
			}

			if err := resp.EncodeProp(code, val); err != nil {
				return nil, err
			}
		}
	} else if prop := propfind.Prop; prop != nil {
		for _, raw := range prop.Raw {
			xmlName, ok := raw.XMLName()
			if !ok {
				continue
			}

			emptyVal := NewRawXMLElement(xmlName, nil, nil)

			var code int
			var val interface{} = emptyVal
			f, ok := props[xmlName]
			if ok {
				if v, err := f(&raw); err != nil {
					// TODO: don't throw away error message here
					code = HTTPErrorFromError(err).Code
				} else {
					code = http.StatusOK
					val = v
				}
			} else {
				code = http.StatusNotFound
			}

			if err := resp.EncodeProp(code, val); err != nil {
				return nil, err
			}
		}
	} else {
		return nil, HTTPErrorf(http.StatusBadRequest, "webdav: request missing propname, allprop or prop element")
	}

	return resp, nil
}

func (h *Handler) handleProppatch(c *fiber.Ctx) error {
	var update PropertyUpdate
	if err := DecodeXMLRequest(c, &update); err != nil {
		return err
	}

	resp, err := h.Backend.PropPatch(c, &update)
	if err != nil {
		return err
	}

	ms := NewMultiStatus(*resp)
	return ServeMultiStatus(c, ms)
}

func parseDestination(c *fiber.Ctx) (*Href, error) {
	destHref := c.Get("Destination")
	if destHref == "" {
		return nil, HTTPErrorf(fiber.StatusBadRequest, "webdav: missing Destination header in MOVE request")
	}
	dest, err := url.Parse(destHref)
	if err != nil {
		return nil, HTTPErrorf(fiber.StatusBadRequest, "webdav: marlformed Destination header in MOVE request: %v", err)
	}
	return (*Href)(dest), nil
}

func (h *Handler) handleCopyMove(c *fiber.Ctx) error {
	dest, err := parseDestination(c)
	if err != nil {
		return err
	}

	overwrite := true
	if s := c.Get("Overwrite"); s != "" {
		overwrite, err = ParseOverwrite(s)
		if err != nil {
			return err
		}
	}

	depth := DepthInfinity
	if s := c.Get("Depth"); s != "" {
		depth, err = ParseDepth(s)
		if err != nil {
			return err
		}
	}

	var created bool
	if c.Method() == "COPY" {
		var recursive bool
		switch depth {
		case DepthZero:
			recursive = false
		case DepthOne:
			return HTTPErrorf(fiber.StatusBadRequest, `webdav: "Depth: 1" is not supported in COPY request`)
		case DepthInfinity:
			recursive = true
		}

		created, err = h.Backend.Copy(c, dest, recursive, overwrite)
	} else {
		if depth != DepthInfinity {
			return HTTPErrorf(fiber.StatusBadRequest, `webdav: only "Depth: infinity" is accepted in MOVE request`)
		}
		created, err = h.Backend.Move(c, dest, overwrite)
	}
	if err != nil {
		return err
	}

	if created {
		c.Status(fiber.StatusCreated)
	} else {
		c.Status(fiber.StatusNoContent)
	}
	return nil
}

func (h *Handler) handleLock(c *fiber.Ctx) error {
	var (
		lockInfo     LockInfo
		refreshToken string
	)
	if isContentXML(c) {
		if err := DecodeXMLRequest(c, &lockInfo); err != nil {
			return err
		}
	} else {
		if err := ensureRequestBodyEmpty(c); err != nil {
			return err
		}

		conditions, err := ParseConditions(c.Get("If"))
		if err != nil {
			return &HTTPError{fiber.StatusBadRequest, err}
		} else if len(conditions) != 1 || len(conditions[0]) != 1 || conditions[0][0].Token == "" {
			return HTTPErrorf(fiber.StatusBadRequest, "webdav: a single lock token must be specified in the If header field")
		}
		refreshToken = conditions[0][0].Token
	}

	if lockInfo.LockScope.Exclusive == nil || lockInfo.LockScope.Shared != nil {
		return HTTPErrorf(fiber.StatusBadRequest, "webdav: only exclusive locks are supported")
	}
	if lockInfo.LockType.Write == nil {
		return HTTPErrorf(fiber.StatusBadRequest, "webdav: only write locks are supported")
	}

	depth := DepthInfinity
	if s := c.Get("Depth"); s != "" {
		var err error
		depth, err = ParseDepth(s)
		if err != nil {
			return &HTTPError{fiber.StatusBadRequest, err}
		}
	}

	var timeout time.Duration
	if s := c.Get("Timeout"); s != "" {
		t, err := ParseTimeout(s)
		if err != nil {
			return &HTTPError{fiber.StatusBadRequest, err}
		}
		timeout = t.Duration
	}

	lock, created, err := h.Backend.Lock(c, depth, timeout, refreshToken)
	if err != nil {
		return err
	}

	var t *Timeout
	if lock.Timeout != 0 {
		t = &Timeout{Duration: lock.Timeout}
	}

	lockDiscovery := &LockDiscovery{
		ActiveLock: []ActiveLock{
			{
				LockScope: LockScope{
					Exclusive: &struct{}{},
				},
				LockType: LockType{
					Write: &struct{}{},
				},
				Depth:     depth,
				Timeout:   t,
				LockToken: &LockToken{Href: lock.Href},
				LockRoot:  LockRoot{Href: lock.Root},
			},
		},
	}
	prop, err := EncodeProp(lockDiscovery)
	if err != nil {
		return err
	}

	if refreshToken == "" {
		c.Set("Lock-Token", FormatLockToken(lock.Href))
	}
	if created {
		c.Status(fiber.StatusCreated)
	} else {
		c.Status(fiber.StatusOK)
	}
	return ServeXML(c).Encode(prop)
}

func (h *Handler) handleUnlock(c *fiber.Ctx) error {
	tokenHref, err := ParseLockToken(c.Get("Lock-Token"))
	if err != nil {
		return &HTTPError{fiber.StatusBadRequest, err}
	}

	if err := h.Backend.Unlock(c, tokenHref); err != nil {
		return err
	}

	c.Status(fiber.StatusNoContent)
	return nil
}
