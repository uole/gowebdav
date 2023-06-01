package gowebdav

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
)

func (c *Client) req(ctx context.Context, method, path string, body io.Reader, intercept func(*http.Request)) (req *http.Response, err error) {
	var r *http.Request
	var retryBuf io.Reader

	if body != nil {
		// If the authorization fails, we will need to restart reading
		// from the passed body stream.
		// When body is seekable, use seek to reset the streams
		// cursor to the start.
		// Otherwise, copy the stream into a buffer while uploading
		// and use the buffers content on retry.
		if sk, ok := body.(io.Seeker); ok {
			if _, err = sk.Seek(0, io.SeekStart); err != nil {
				return
			}
			retryBuf = body
		} else {
			buff := &bytes.Buffer{}
			retryBuf = buff
			body = io.TeeReader(body, buff)
		}
		r, err = http.NewRequest(method, PathEscape(Join(c.root, path)), body)
	} else {
		r, err = http.NewRequest(method, PathEscape(Join(c.root, path)), nil)
	}

	if err != nil {
		return nil, err
	}

	for k, vals := range c.headers {
		for _, v := range vals {
			r.Header.Add(k, v)
		}
	}

	// make sure we read 'c.auth' only once since it will be substituted below
	// and that is unsafe to do when multiple goroutines are running at the same time.
	c.authMutex.Lock()
	auth := c.auth
	c.authMutex.Unlock()

	auth.Authorize(r, method, path)

	if intercept != nil {
		intercept(r)
	}

	if c.interceptor != nil {
		c.interceptor(method, r)
	}

	rs, err := c.c.Do(r.WithContext(ctx))
	if err != nil {
		return nil, err
	}

	if rs.StatusCode == 401 && auth.Type() == "NoAuth" {
		wwwAuthenticateHeader := strings.ToLower(rs.Header.Get("Www-Authenticate"))

		if strings.Index(wwwAuthenticateHeader, "digest") > -1 {
			c.authMutex.Lock()
			c.auth = &DigestAuth{auth.User(), auth.Pass(), digestParts(rs)}
			c.authMutex.Unlock()
		} else if strings.Index(wwwAuthenticateHeader, "basic") > -1 {
			c.authMutex.Lock()
			c.auth = &BasicAuth{auth.User(), auth.Pass()}
			c.authMutex.Unlock()
		} else {
			return rs, newPathError("Authorize", c.root, rs.StatusCode)
		}

		// retryBuf will be nil if body was nil initially so no check
		// for body == nil is required here.
		return c.req(ctx, method, path, retryBuf, intercept)
	} else if rs.StatusCode == 401 {
		return rs, newPathError("Authorize", c.root, rs.StatusCode)
	}

	return rs, err
}

func (c *Client) mkcol(ctx context.Context, path string) (status int, err error) {
	rs, err := c.req(ctx, "MKCOL", path, nil, nil)
	if err != nil {
		return
	}
	defer rs.Body.Close()

	status = rs.StatusCode
	if status == 405 {
		status = 201
	}

	return
}

func (c *Client) options(ctx context.Context, path string) (*http.Response, error) {
	return c.req(ctx, "OPTIONS", path, nil, func(rq *http.Request) {
		rq.Header.Add("Depth", "0")
	})
}

func (c *Client) propfind(ctx context.Context, path string, self bool, body string, resp interface{}, parse func(resp interface{}) error) error {
	rs, err := c.req(ctx, "PROPFIND", path, strings.NewReader(body), func(rq *http.Request) {
		if self {
			rq.Header.Add("Depth", "0")
		} else {
			rq.Header.Add("Depth", "1")
		}
		rq.Header.Add("Content-Type", "application/xml;charset=UTF-8")
		rq.Header.Add("Accept", "application/xml,text/xml")
		rq.Header.Add("Accept-Charset", "utf-8")
		// TODO add support for 'gzip,deflate;q=0.8,q=0.7'
		rq.Header.Add("Accept-Encoding", "")
	})
	if err != nil {
		return err
	}
	defer rs.Body.Close()

	if rs.StatusCode != 207 {
		return newPathError("PROPFIND", path, rs.StatusCode)
	}

	return parseXML(rs.Body, resp, parse)
}

func (c *Client) doCopyMove(
	ctx context.Context,
	method string,
	oldpath string,
	newpath string,
	overwrite bool,
) (
	status int,
	r io.ReadCloser,
	err error,
) {
	rs, err := c.req(ctx, method, oldpath, nil, func(rq *http.Request) {
		rq.Header.Add("Destination", PathEscape(Join(c.root, newpath)))
		if overwrite {
			rq.Header.Add("Overwrite", "T")
		} else {
			rq.Header.Add("Overwrite", "F")
		}
	})
	if err != nil {
		return
	}
	status = rs.StatusCode
	r = rs.Body
	return
}

func (c *Client) copymove(ctx context.Context, method string, oldpath string, newpath string, overwrite bool) (err error) {
	s, data, err := c.doCopyMove(ctx, method, oldpath, newpath, overwrite)
	if err != nil {
		return
	}
	if data != nil {
		defer data.Close()
	}

	switch s {
	case 201, 204:
		return nil

	case 207:
		// TODO handle multistat errors, worst case ...
		log.Printf("TODO handle %s - %s multistatus result %s\n", method, oldpath, String(data))

	case 409:
		err := c.createParentCollection(ctx, newpath)
		if err != nil {
			return err
		}

		return c.copymove(ctx, method, oldpath, newpath, overwrite)
	}

	return newPathError(method, oldpath, s)
}

func (c *Client) put(ctx context.Context, path string, stream io.Reader) (status int, err error) {
	rs, err := c.req(ctx, "PUT", path, stream, nil)
	if err != nil {
		return
	}
	defer rs.Body.Close()

	status = rs.StatusCode
	return
}

func (c *Client) createParentCollection(ctx context.Context, itemPath string) (err error) {
	parentPath := path.Dir(itemPath)
	if parentPath == "." || parentPath == "/" {
		return nil
	}
	return c.MkdirAll(ctx, parentPath, 0755)
}
