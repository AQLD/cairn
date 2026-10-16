package objstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3 is an S3-compatible bucket, addressed path-style (endpoint/bucket/key),
// which every S3-compatible service accepts.
type S3 struct {
	Endpoint     *url.URL
	Region       string
	Bucket       string
	AccessKey    string
	SecretKey    string
	SessionToken string
	Client       *http.Client // http.DefaultClient if nil
	// PartSize: objects larger than this are uploaded in parts of this size
	// (default 64 MiB; S3 allows at most 5 GiB in one PUT).
	PartSize int64
	// Attempts per request before giving up (default 5), with backoff
	// between them; a dropped connection or a 5xx is retried.
	Attempts int
	// Now is the clock used for signatures (time.Now if nil).
	Now func() time.Time
}

func (s *S3) String() string { return "s3://" + s.Bucket }

func (s *S3) Put(ctx context.Context, key string, r io.ReaderAt, size int64) error {
	part := s.PartSize
	if part <= 0 {
		part = 64 << 20
	}
	if size <= part {
		_, err := s.do(ctx, http.MethodPut, key, nil, r, 0, size, nil)
		return err
	}
	return s.putMultipart(ctx, key, r, size, part)
}

func (s *S3) putMultipart(ctx context.Context, key string, r io.ReaderAt, size, part int64) error {
	resp, err := s.do(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil, 0, 0, nil)
	if err != nil {
		return err
	}
	var start struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(resp, &start); err != nil || start.UploadID == "" {
		return fmt.Errorf("start multipart upload of %s: bad reply %q", key, truncate(resp))
	}
	type completed struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	}
	var parts []completed
	for n, off := 1, int64(0); off < size; n, off = n+1, off+part {
		length := min(part, size-off)
		var etag string
		q := url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {start.UploadID}}
		if _, err := s.do(ctx, http.MethodPut, key, q, r, off, length, func(h http.Header) { etag = h.Get("ETag") }); err != nil {
			s.abort(key, start.UploadID)
			return err
		}
		parts = append(parts, completed{PartNumber: n, ETag: etag})
	}
	body, err := xml.Marshal(struct {
		XMLName xml.Name    `xml:"CompleteMultipartUpload"`
		Parts   []completed `xml:"Part"`
	}{Parts: parts})
	if err != nil {
		return err
	}
	resp, err = s.do(ctx, http.MethodPost, key, url.Values{"uploadId": {start.UploadID}}, bytes.NewReader(body), 0, int64(len(body)), nil)
	if err != nil {
		s.abort(key, start.UploadID)
		return err
	}
	// S3 can answer 200 and still report a failure in the body
	if bytes.Contains(resp, []byte("<Error>")) {
		s.abort(key, start.UploadID)
		return fmt.Errorf("complete multipart upload of %s: %s", key, truncate(resp))
	}
	return nil
}

// abort cleans up a failed multipart upload, best effort, on its own context:
// the caller's may be the reason it failed.
func (s *S3) abort(key, uploadID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = s.do(ctx, http.MethodDelete, key, url.Values{"uploadId": {uploadID}}, nil, 0, 0, nil)
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	var body io.ReadCloser
	err := s.retry(ctx, func() (bool, error) {
		req, err := s.request(ctx, http.MethodGet, key, nil, nil, 0)
		if err != nil {
			return false, err
		}
		resp, err := s.client().Do(req)
		if err != nil {
			return true, err
		}
		if resp.StatusCode == http.StatusOK {
			body = resp.Body
			return false, nil
		}
		return s.failure(resp, key)
	})
	return body, err
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.do(ctx, http.MethodDelete, key, nil, nil, 0, 0, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, http.MethodGet, "", q, nil, 0, 0, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			IsTruncated bool   `xml:"IsTruncated"`
			Next        string `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(resp, &page); err != nil {
			return nil, fmt.Errorf("list %s: %v", prefix, err)
		}
		for _, c := range page.Contents {
			keys = append(keys, c.Key)
		}
		if !page.IsTruncated || page.Next == "" {
			return keys, nil
		}
		token = page.Next
	}
}

// do sends one request (retried) and returns the response body. A body is
// read from r at [off, off+length) afresh on every attempt.
func (s *S3) do(ctx context.Context, method, key string, q url.Values, r io.ReaderAt, off, length int64, headers func(http.Header)) ([]byte, error) {
	var out []byte
	err := s.retry(ctx, func() (bool, error) {
		var body io.Reader
		if r != nil {
			body = io.NewSectionReader(r, off, length)
		}
		req, err := s.request(ctx, method, key, q, body, length)
		if err != nil {
			return false, err
		}
		resp, err := s.client().Do(req)
		if err != nil {
			return true, err
		}
		if resp.StatusCode/100 != 2 {
			return s.failure(resp, key)
		}
		defer resp.Body.Close()
		out, err = io.ReadAll(resp.Body)
		if err != nil {
			return true, err
		}
		if headers != nil {
			headers(resp.Header)
		}
		return false, nil
	})
	return out, err
}

// failure turns an error response into an error, and says whether to retry.
func (s *S3) failure(resp *http.Response, key string) (bool, error) {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	err := fmt.Errorf("%s %s: %s: %s", resp.Request.Method, key, resp.Status, truncate(body))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, fmt.Errorf("%w: %v", ErrNotFound, err)
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusRequestTimeout:
		return true, err
	default:
		return false, err
	}
}

// retry runs attempt until it succeeds, says not to retry, or the attempts
// run out, backing off between tries.
func (s *S3) retry(ctx context.Context, attempt func() (retry bool, err error)) error {
	n := s.Attempts
	if n <= 0 {
		n = 5
	}
	delay := 200 * time.Millisecond
	var err error
	for i := 0; i < n; i++ {
		var again bool
		if again, err = attempt(); err == nil || !again {
			return err
		}
		if i == n-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 10*time.Second)
	}
	return fmt.Errorf("after %d attempts: %w", n, err)
}

func (s *S3) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

// request builds a request signed with AWS Signature Version 4. The payload
// is not hashed (UNSIGNED-PAYLOAD): saves are verified by their own checksums.
func (s *S3) request(ctx context.Context, method, key string, q url.Values, body io.Reader, length int64) (*http.Request, error) {
	path := "/" + s.Bucket
	if key != "" {
		path += "/" + key
	}
	u := *s.Endpoint
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawPath = strings.TrimSuffix(s.Endpoint.EscapedPath(), "/") + escapePath(path)
	u.RawQuery = canonicalQuery(q)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = length
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	t := now().UTC()
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	if s.SessionToken != "" {
		req.Header.Set("x-amz-security-token", s.SessionToken)
	}
	names := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if s.SessionToken != "" {
		names = append(names, "x-amz-security-token")
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, h := range names {
		v := req.Header.Get(h)
		if h == "host" {
			v = req.URL.Host
		}
		canonHeaders.WriteString(h + ":" + strings.TrimSpace(v) + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{method, u.EscapedPath(), u.RawQuery, canonHeaders.String(), signed, "UNSIGNED-PAYLOAD"}, "\n")
	scope := day + "/" + s.Region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hexSHA256([]byte(canonical))
	k := hmacSHA256([]byte("AWS4"+s.SecretKey), day)
	k = hmacSHA256(k, s.Region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", s.AccessKey, scope, signed, sig))
	return req, nil
}

// escapePath URI-encodes each path segment the way SigV4 wants (RFC 3986
// unreserved characters kept, everything else percent-encoded, '/' kept).
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string{}, q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func hexSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
