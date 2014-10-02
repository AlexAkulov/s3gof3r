package s3gof3r

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"hash"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// defined by amazon
const (
	minPartSize = 5 * mb
	maxPartSize = 5 * gb
	maxObjSize  = 5 * tb
	maxNPart    = 10000
	md5Header   = "content-md5"
)

type part struct {
	r   io.ReadSeeker
	len int64
	b   *bytes.Buffer

	// read by xml encoder
	PartNumber int
	ETag       string

	// Used for checksum of checksums on completion
	contentMd5 string
}

type putter struct {
	url url.URL
	b   *Bucket
	c   *Config

	bufsz      int64
	buf        *bytes.Buffer
	ch         chan *part
	part       int
	closed     bool
	err        error
	wg         sync.WaitGroup
	md5OfParts hash.Hash
	md5        hash.Hash
	ETag       string

	bp *bp

	makes    int
	UploadId string // casing matches s3 xml
	xml      struct {
		XMLName string `xml:"CompleteMultipartUpload"`
		Part    []*part
	}
}

// Sends an S3 multipart upload initiation request.
// See http://docs.amazonwebservices.com/AmazonS3/latest/dev/mpuoverview.html.
// The initial request returns an UploadId that we use to identify
// subsequent PUT requests.
func newPutter(url url.URL, h http.Header, c *Config, b *Bucket) (p *putter, err error) {
	p = new(putter)
	p.url = url
	p.b = b
	p.c = c
	p.c.Concurrency = max(c.Concurrency, 1)
	p.c.NTry = max(c.NTry, 1)
	p.bufsz = max64(minPartSize, c.PartSize)
	resp, err := p.retryRequest("POST", url.String()+"?uploads", nil, h)
	if err != nil {
		return nil, err
	}
	defer checkClose(resp.Body, &err)
	if resp.StatusCode != 200 {
		return nil, newRespError(resp)
	}
	err = xml.NewDecoder(resp.Body).Decode(p)
	if err != nil {
		return nil, err
	}
	p.ch = make(chan *part)
	for i := 0; i < p.c.Concurrency; i++ {
		go p.worker()
	}
	p.md5OfParts = md5.New()
	p.md5 = md5.New()

	p.bp = newBufferPool(p.bufsz)

	return p, nil
}

func (p *putter) Write(b []byte) (int, error) {
	if p.closed {
		p.abort()
		return 0, syscall.EINVAL
	}
	if p.err != nil {
		p.abort()
		return 0, p.err
	}
	if p.buf == nil {
		p.buf = <-p.bp.get
		// grow to bufsz, allocating overhead to avoid slice growth
		p.buf.Grow(int(p.bufsz + 100*kb))
	}
	n, err := p.buf.Write(b)
	if err != nil {
		p.abort()
		return n, err
	}

	if int64(p.buf.Len()) >= p.bufsz {
		p.flush()
	}
	return n, nil
}

func (p *putter) flush() {
	p.wg.Add(1)
	p.part++
	b := *p.buf
	part := &part{bytes.NewReader(b.Bytes()), int64(b.Len()), p.buf, p.part, "", ""}
	var err error
	part.contentMd5, part.ETag, err = p.md5Content(part.r)
	if err != nil {
		p.err = err
	}

	p.xml.Part = append(p.xml.Part, part)
	p.ch <- part
	p.buf = nil
	// double buffer size every 1000 parts to
	// avoid exceeding the 10000-part AWS limit
	// while still reaching the 5 Terabyte max object size
	if p.part%1000 == 0 {
		p.bufsz = min64(p.bufsz*2, maxPartSize)
		p.bp.makeSize = p.bufsz
		logger.debugPrintf("part size doubled to %d", p.bufsz)

	}

}

func (p *putter) worker() {
	for part := range p.ch {
		p.retryPutPart(part)
	}
}

// Calls putPart up to nTry times to recover from transient errors.
func (p *putter) retryPutPart(part *part) {
	defer p.wg.Done()
	var err error
	for i := 0; i < p.c.NTry; i++ {
		time.Sleep(time.Duration(math.Exp2(float64(i))) * 100 * time.Millisecond) // exponential back-off
		err = p.putPart(part)
		if err == nil {
			p.bp.give <- part.b
			return
		}
		logger.debugPrintf("Error on attempt %d: Retrying part: %v, Error: %s", i, part, err)
	}
	p.err = err
}

// uploads a part, checking the etag against the calculated value
func (p *putter) putPart(part *part) error {
	v := url.Values{}
	v.Set("partNumber", strconv.Itoa(part.PartNumber))
	v.Set("uploadId", p.UploadId)
	if _, err := part.r.Seek(0, 0); err != nil { // move back to beginning, if retrying
		return err
	}
	req, err := http.NewRequest("PUT", p.url.String()+"?"+v.Encode(), part.r)
	if err != nil {
		return err
	}
	req.ContentLength = part.len
	req.Header.Set(md5Header, part.contentMd5)
	p.b.Sign(req)
	resp, err := p.c.Client.Do(req)
	if err != nil {
		return err
	}
	defer checkClose(resp.Body, &err)
	if resp.StatusCode != 200 {
		return newRespError(resp)
	}
	s := resp.Header.Get("etag")
	s = s[1 : len(s)-1] // includes quote chars for some reason
	if part.ETag != s {
		return fmt.Errorf("Response etag does not match. Remote:%s Calculated:%s", s, p.ETag)
	}
	return nil
}

func (p *putter) Close() (err error) {
	if p.closed {
		p.abort()
		return syscall.EINVAL
	}
	if p.buf != nil {
		buf := *p.buf
		if buf.Len() > 0 {
			p.flush()
		}
	}
	p.wg.Wait()
	close(p.ch)
	p.closed = true
	close(p.bp.quit)

	if p.part == 0 {
		p.abort()
		return fmt.Errorf("0 bytes written")
	}
	if p.err != nil {
		p.abort()
		return p.err
	}
	// Complete Multipart upload
	body, err := xml.Marshal(p.xml)
	if err != nil {
		p.abort()
		return
	}
	b := bytes.NewReader(body)
	v := url.Values{}
	v.Set("uploadId", p.UploadId)
	resp, err := p.retryRequest("POST", p.url.String()+"?"+v.Encode(), b, nil)
	if err != nil {
		p.abort()
		return
	}
	defer checkClose(resp.Body, &err)
	if resp.StatusCode != 200 {
		p.abort()
		return newRespError(resp)
	}
	// Check md5 hash of concatenated part md5 hashes against ETag
	// more info: https://forums.aws.amazon.com/thread.jspa?messageID=456442&#456442
	calculatedMd5ofParts := fmt.Sprintf("%x", p.md5OfParts.Sum(nil))
	// Parse etag from body of response
	err = xml.NewDecoder(resp.Body).Decode(p)
	if err != nil {
		return
	}
	// strip part count from end and '"' from front.
	remoteMd5ofParts := strings.Split(p.ETag, "-")[0]
	remoteMd5ofParts = remoteMd5ofParts[1:len(remoteMd5ofParts)]
	if calculatedMd5ofParts != remoteMd5ofParts {
		if err != nil {
			return err
		}
		return fmt.Errorf("MD5 hash of part hashes comparison failed. Hash from multipart complete header: %s."+
			" Calculated multipart hash: %s.", remoteMd5ofParts, calculatedMd5ofParts)
	}
	if p.c.Md5Check {
		for i := 0; i < p.c.NTry; i++ {
			if err = p.putMd5(); err == nil {
				break
			}
		}
		return
	}
	return
}

// Try to abort multipart upload. Do not error on failure.
func (p *putter) abort() {
	v := url.Values{}
	v.Set("uploadId", p.UploadId)
	s := p.url.String() + "?" + v.Encode()
	resp, err := p.retryRequest("DELETE", s, nil, nil)
	if err != nil {
		logger.Printf("Error aborting multipart upload: %v\n", err)
		return
	}
	defer checkClose(resp.Body, &err)
	if resp.StatusCode != 204 {
		logger.Printf("Error aborting multipart upload: %v", newRespError(resp))
	}
	return
}

// Md5 functions
func (p *putter) md5Content(r io.ReadSeeker) (string, string, error) {
	h := md5.New()
	mw := io.MultiWriter(h, p.md5)
	if _, err := io.Copy(mw, r); err != nil {
		return "", "", err
	}
	sum := h.Sum(nil)
	hexSum := fmt.Sprintf("%x", sum)
	// add to checksum of all parts for verification on upload completion
	if _, err := p.md5OfParts.Write(sum); err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(sum), hexSum, nil
}

// Put md5 file in .md5 subdirectory of bucket  where the file is stored
// e.g. the md5 for https://mybucket.s3.amazonaws.com/gof3r will be stored in
// https://mybucket.s3.amazonaws.com/.md5/gof3r.md5
func (p *putter) putMd5() (err error) {
	calcMd5 := fmt.Sprintf("%x", p.md5.Sum(nil))
	md5Reader := strings.NewReader(calcMd5)
	md5Path := fmt.Sprint(".md5", p.url.Path, ".md5")
	md5Url, err := p.b.url(md5Path)
	if err != nil {
		return err
	}
	logger.debugPrintln("md5: ", calcMd5)
	logger.debugPrintln("md5Path: ", md5Path)
	r, err := http.NewRequest("PUT", md5Url.String(), md5Reader)
	if err != nil {
		return
	}
	p.b.Sign(r)
	resp, err := p.c.Client.Do(r)
	if err != nil {
		return
	}
	defer checkClose(resp.Body, &err)
	if resp.StatusCode != 200 {
		return newRespError(resp)
	}
	return
}

func (p *putter) retryRequest(method, urlStr string, body io.ReadSeeker, h http.Header) (resp *http.Response, err error) {
	for i := 0; i < p.c.NTry; i++ {
		var req *http.Request
		req, err = http.NewRequest(method, urlStr, body)
		if err != nil {
			return
		}
		for k := range h {
			for _, v := range h[k] {
				req.Header.Add(k, v)
			}
		}

		p.b.Sign(req)
		resp, err = p.c.Client.Do(req)
		if err == nil {
			return
		}
		logger.debugPrintln(err)
		if body != nil {
			if _, err = body.Seek(0, 0); err != nil {
				return
			}
		}
	}
	return
}
