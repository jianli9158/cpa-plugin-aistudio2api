package api

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"golang.org/x/sync/errgroup"
)

// remoteMediaUserAgent 是下载外部媒体时声明的客户端标识
const remoteMediaUserAgent = "AIStudio2API (+https://github.com/Mag1cFall/AIStudio2API)"

var remoteMediaValidator = outboundValidator{
	kind:             "remote_media",
	requireAllowlist: false,
	allowRedirects:   true,
	maxRedirects:     10,
	envProxy:         true,
}

// remoteMediaClient 是下载外部媒体的 HTTP 客户端，每次请求、重定向与底层建连均校验公网地址与协议
var remoteMediaClient = newRemoteMediaClient()

func newRemoteMediaClient() *http.Client {
	client, err := remoteMediaValidator.newHTTPClient(0, nil)
	if err != nil {
		panic(err)
	}
	return client
}

// remoteMediaProxy 判断连接地址是否为进程环境变量中 http 或 https 请求的代理
func remoteMediaProxy(address string) bool {
	for _, scheme := range []string{"http", "https"} {
		proxy, _ := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: scheme, Host: "remote-media.invalid"}})
		if proxy != nil && net.JoinHostPort(proxy.Hostname(), cmp.Or(proxy.Port(), proxyDefaultPorts[proxy.Scheme])) == address {
			return true
		}
	}
	return false
}

// proxyDefaultPorts 为代理 URL 省略端口时各协议的端口
var proxyDefaultPorts = map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}

// remoteMediaConcurrency 是一次请求同时下载的外部媒体 URL 数上限
const remoteMediaConcurrency = 4

// inlineRemoteMedia 把内容中 http(s) URL 形式的文件引用下载为内联媒体，同一 URL 只下载一次，不同 URL 最多 remoteMediaConcurrency 个同时下载，一个失败时取消其余下载，Gemini Files API 的文件 URI 保持文件引用
func inlineRemoteMedia(ctx context.Context, contents []aistudio.Content) error {
	var parts []*aistudio.Part
	// first 是每个 URL 第一次出现的片段，下载时取它声明的 MIME
	var first []*aistudio.Part
	index := make(map[string]int)
	for contentIndex := range contents {
		for partIndex := range contents[contentIndex].Parts {
			part := &contents[contentIndex].Parts[partIndex]
			if part.File == nil || !remoteMediaURL(part.File.ID) || geminiFilesAPIURI(part.File.ID) {
				continue
			}
			parts = append(parts, part)
			if _, ok := index[part.File.ID]; !ok {
				index[part.File.ID] = len(first)
				first = append(first, part)
			}
		}
	}
	blobs := make([]*aistudio.Blob, len(first))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(remoteMediaConcurrency)
	for i, part := range first {
		url, declared := part.File.ID, part.File.MIME
		group.Go(func() error {
			var err error
			if blobs[i], err = downloadRemoteMedia(groupCtx, url, declared); err != nil {
				return fmt.Errorf("Error while downloading %s: %w", url, err)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	for _, part := range parts {
		blob := blobs[index[part.File.ID]]
		part.DisplayName = cmp.Or(part.DisplayName, part.File.Name)
		part.File, part.InlineData = nil, blob
	}
	return nil
}

// remoteMediaURL 返回值是否为 http 或 https URL
func remoteMediaURL(value string) bool {
	lower := strings.ToLower(value)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// checkRemoteMediaHost 拒绝解析到本机、私网、链路本地、CGNAT、组播、未指定或保留地址的主机
func checkRemoteMediaHost(ctx context.Context, host string) error {
	_, err := remoteMediaValidator.resolveAndCheckHost(ctx, host)
	return err
}

// checkRemoteMediaAddress 拒绝全部非公网 IP 地址
func checkRemoteMediaAddress(address netip.Addr) error {
	return CheckPublicIP(address)
}

// downloadRemoteMedia 在请求 context 内下载外部媒体，MIME 依次取响应 Content-Type、声明值与内容判断
func downloadRemoteMedia(ctx context.Context, rawURL string, declared string) (*aistudio.Blob, error) {
	if _, _, err := remoteMediaValidator.validateURL(ctx, rawURL); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", remoteMediaUserAgent)
	response, err := remoteMediaClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("HTTP %s", response.Status)
	}
	if response.ContentLength > openAIFileMaxBytes {
		return nil, fmt.Errorf("content exceeds %d bytes", openAIFileMaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, openAIFileMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > openAIFileMaxBytes {
		return nil, fmt.Errorf("content exceeds %d bytes", openAIFileMaxBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("response body is empty")
	}
	mimeType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mimeType == "application/octet-stream" || mimeType == "binary/octet-stream" {
		mimeType = declared
	}
	mimeType, data = normalizeImagePayload(attachmentMIME(mimeType, "", data), data)
	return &aistudio.Blob{MIME: mimeType, Data: data}, nil
}
