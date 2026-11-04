package aws

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

var validBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// s3URL is where a bucket, or an object in it, is. It names the bucket in
// the host, as AWS prefers, unless the name has dots, which S3's TLS
// certificate does not cover.
func (c client) s3URL(bucket, key string, q url.Values) string {
	host, path := "https://"+bucket+".s3."+c.region+".amazonaws.com", "/"+key
	if c.a.base != "" || strings.Contains(bucket, ".") {
		host, path = c.endpoint("s3"), "/"+bucket
		if key != "" {
			path += "/" + key
		}
	}
	u := host + uriEncode(path, true)
	if len(q) > 0 {
		u += "?" + encodeQuery(q)
	}
	return u
}

// bucket checks a bucket is in the client's region, which keeps every S3
// call within the binding's region. It asks S3 once an hour per bucket.
func (c client) bucket(ctx context.Context, name string) error {
	if !validBucket.MatchString(name) {
		return fmt.Errorf("%q is not an S3 bucket's name", web.Short(name, 70))
	}
	now := c.a.now()
	region, ok := c.a.buckets.Get(name, now)
	if !ok {
		resp, err := c.send(ctx, "s3", http.MethodHead, c.s3URL(name, "", nil), nil, nil)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		// S3 names the region even when it refuses the request, or redirects
		// it to the bucket's region.
		region = resp.Header.Get("X-Amz-Bucket-Region")
		switch {
		case region != "":
			c.a.buckets.Put(name, region, now)
		case resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("there is no S3 bucket called %s", name)
		default:
			return &web.APIError{Service: labels["s3"], Status: resp.StatusCode, Message: "could not find the bucket's region"}
		}
	}
	if region != c.region {
		return fmt.Errorf("the bucket %s is in %s, outside this agent's region, %s", name, region, c.region)
	}
	return nil
}

type bucket struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

type buckets struct {
	Buckets []bucket `json:"buckets"`
	More    bool     `json:"more,omitempty"`
}

func s3Buckets(ctx context.Context, c client, in struct {
	Limit int `json:"limit"`
},
) (any, error) {
	q := url.Values{"bucket-region": {c.region}, "max-buckets": {strconv.Itoa(apitool.Limit(in.Limit, 25, 100))}}
	host := "https://s3." + c.region + ".amazonaws.com"
	if c.a.base != "" {
		host = c.a.base
	}
	var out struct {
		Buckets []struct {
			Name    string    `xml:"Name"`
			Created time.Time `xml:"CreationDate"`
		} `xml:"Buckets>Bucket"`
		Next string `xml:"ContinuationToken"`
	}
	if err := c.getXML(ctx, host+"/?"+encodeQuery(q), &out); err != nil {
		return nil, err
	}
	res := buckets{Buckets: []bucket{}, More: out.Next != ""}
	for _, b := range out.Buckets {
		res.Buckets = append(res.Buckets, bucket{Name: b.Name, Created: b.Created})
	}
	return res, nil
}

func (c client) getXML(ctx context.Context, u string, out any) error {
	b, err := c.do(ctx, "s3", http.MethodGet, u, nil, nil)
	if err != nil {
		return err
	}
	return xml.Unmarshal(b, out)
}

type object struct {
	Key      string    `json:"key"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type listing struct {
	Folders []string `json:"folders,omitempty"`
	Objects []object `json:"objects"`
	// Next is passed back as after for the next page.
	Next string `json:"next,omitempty"`
}

type listArgs struct {
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	Recursive bool   `json:"recursive"`
	Limit     int    `json:"limit"`
	After     string `json:"after"`
}

func s3List(ctx context.Context, c client, in listArgs) (any, error) {
	if err := c.bucket(ctx, in.Bucket); err != nil {
		return nil, err
	}
	q := url.Values{"list-type": {"2"}, "max-keys": {strconv.Itoa(apitool.Limit(in.Limit, 25, 100))}}
	if in.Prefix != "" {
		q.Set("prefix", in.Prefix)
	}
	if !in.Recursive {
		q.Set("delimiter", "/")
	}
	if in.After != "" {
		q.Set("continuation-token", in.After)
	}
	var out struct {
		Contents []struct {
			Key      string    `xml:"Key"`
			Size     int64     `xml:"Size"`
			Modified time.Time `xml:"LastModified"`
		} `xml:"Contents"`
		Prefixes []string `xml:"CommonPrefixes>Prefix"`
		Next     string   `xml:"NextContinuationToken"`
	}
	if err := c.getXML(ctx, c.s3URL(in.Bucket, "", q), &out); err != nil {
		return nil, err
	}
	res := listing{Folders: out.Prefixes, Objects: []object{}, Next: out.Next}
	for _, o := range out.Contents {
		res.Objects = append(res.Objects, object{Key: o.Key, Size: o.Size, Modified: o.Modified})
	}
	return res, nil
}

type objectArgs struct {
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	Content     string `json:"content"`
	ContentType string `json:"content_type"`
	Offset      int64  `json:"offset"`
}

// open checks the bucket and the key.
func (c client) open(ctx context.Context, in objectArgs) error {
	if err := apitool.Need("key", in.Key); err != nil {
		return err
	}
	if len(in.Key) > 1024 {
		return errors.New("S3 keys are at most 1024 bytes long")
	}
	return c.bucket(ctx, in.Bucket)
}

type content struct {
	Bucket      string    `json:"bucket"`
	Key         string    `json:"key"`
	ContentType string    `json:"content_type,omitempty"`
	Modified    time.Time `json:"modified,omitzero"`
	web.Chunk
}

func s3Read(ctx context.Context, c client, in objectArgs) (any, error) {
	if err := c.open(ctx, in); err != nil {
		return nil, err
	}
	if in.Offset < 0 {
		return nil, errors.New("offset cannot be negative")
	}
	h := http.Header{}
	h.Set("Range", web.Range(in.Offset))
	resp, err := c.send(ctx, "s3", http.MethodGet, c.s3URL(in.Bucket, in.Key, nil), nil, h)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("s3://%s/%s is shorter than the offset %d", in.Bucket, in.Key, in.Offset)
	}
	if resp.StatusCode/100 != 2 {
		b, _ := web.ReadBody(resp, 1<<20)
		return nil, apiError("s3", resp.StatusCode, b)
	}
	total := web.RangeTotal(resp)
	offset := in.Offset
	// A server that ignores Range sends it all from the start.
	if resp.StatusCode == http.StatusOK {
		offset = 0
	}
	b, err := web.ReadBody(resp, web.ChunkBytes)
	if errors.Is(err, web.ErrTooLarge) {
		// Only the first chunk is wanted; the rest was not read.
		err = nil
	}
	if err != nil {
		return nil, err
	}
	ct := resp.Header.Get("Content-Type")
	chunk, ok := web.TextChunk(b, offset, max(total, offset+int64(len(b))))
	if !ok {
		return nil, fmt.Errorf("s3://%s/%s is not text (%s, %s)", in.Bucket, in.Key, ct, size(total))
	}
	modified, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	return content{Bucket: in.Bucket, Key: in.Key, ContentType: ct, Modified: modified, Chunk: chunk}, nil
}

func size(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
}

type written struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	Size   int    `json:"size"`
	ETag   string `json:"etag,omitempty"`
}

func s3Write(ctx context.Context, c client, in objectArgs) (any, error) {
	if err := c.open(ctx, in); err != nil {
		return nil, err
	}
	if len(in.Content) > channels.MaxFileBytes {
		return nil, fmt.Errorf("the content is %s; s3_write writes at most %d MiB", size(int64(len(in.Content))), channels.MaxFileBytes>>20)
	}
	ct := strings.TrimSpace(in.ContentType)
	if ct == "" {
		ct = "text/plain; charset=utf-8"
	}
	h := http.Header{}
	h.Set("Content-Type", ct)
	resp, err := c.send(ctx, "s3", http.MethodPut, c.s3URL(in.Bucket, in.Key, nil), []byte(in.Content), h)
	if err != nil {
		return nil, err
	}
	b, _ := web.ReadBody(resp, 1<<20)
	if resp.StatusCode/100 != 2 {
		return nil, apiError("s3", resp.StatusCode, b)
	}
	return written{Bucket: in.Bucket, Key: in.Key, Size: len(in.Content), ETag: strings.Trim(resp.Header.Get("ETag"), `"`)}, nil
}
