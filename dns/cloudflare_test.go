package dns

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/util"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip implements http.RoundTripper for Cloudflare request tests.
func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// cloudflareTestResponse 只返回内存响应，测试不会访问真实 Cloudflare。
func cloudflareTestResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func cloudflareTestProvider(recordType, ipAddr string, domains ...*config.Domain) *Cloudflare {
	cf := &Cloudflare{TTL: 1}
	cf.Domains.Ipv4Cache = &util.IpCache{Addr: ipAddr, Times: 999}
	cf.Domains.Ipv6Cache = &util.IpCache{Addr: ipAddr, Times: 999}
	if recordType == "AAAA" {
		cf.Domains.Ipv6Addr, cf.Domains.Ipv6Domains = ipAddr, domains
	} else {
		cf.Domains.Ipv4Addr, cf.Domains.Ipv4Domains = ipAddr, domains
	}
	return cf
}

func TestCloudflareReconcilesRemoteDriftEveryCycle(t *testing.T) {
	for _, tt := range []struct {
		recordType string
		ipAddr     string
		remoteIP   string
	}{
		{"A", "192.0.2.1", "192.0.2.2"},
		{"AAAA", "2001:db8::1", "2001:db8::2"},
	} {
		t.Run(tt.recordType, func(t *testing.T) {
			domain := &config.Domain{DomainName: "example.com", SubDomain: "home"}
			cf := cloudflareTestProvider(tt.recordType, tt.ipAddr, domain)
			remoteIP := tt.ipAddr
			zoneReads, recordReads, updates := 0, 0, 0
			cf.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/client/v4/zones":
					zoneReads++
					if got := request.URL.Query().Get("name"); got != domain.DomainName {
						t.Errorf("zone name = %q, want %q", got, domain.DomainName)
					}
					return cloudflareTestResponse(`{"success":true,"result":[{"id":"zone-id"}]}`), nil
				case request.Method == http.MethodGet && request.URL.Path == "/client/v4/zones/zone-id/dns_records":
					recordReads++
					if got := request.URL.Query().Get("type"); got != tt.recordType {
						t.Errorf("record type = %q, want %q", got, tt.recordType)
					}
					if got := request.URL.Query().Get("name"); got != domain.ToASCII() {
						t.Errorf("record name = %q, want %q", got, domain.ToASCII())
					}
					return cloudflareTestResponse(`{"success":true,"result":[{"id":"record-id","type":"` + tt.recordType + `","name":"home.example.com","content":"` + remoteIP + `","proxied":true}]}`), nil
				case request.Method == http.MethodPut && request.URL.Path == "/client/v4/zones/zone-id/dns_records/record-id":
					updates++
					var record CloudflareRecord
					if err := json.NewDecoder(request.Body).Decode(&record); err != nil {
						t.Fatalf("decode update: %v", err)
					}
					if record.Content != tt.ipAddr || record.Type != tt.recordType || !record.Proxied {
						t.Errorf("unexpected update: %+v", record)
					}
					remoteIP = record.Content
					return cloudflareTestResponse(`{"success":true}`), nil
				default:
					t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
					return nil, errors.New("unexpected request")
				}
			})}

			// 已缓存的本机 IP 保持不变：先核对，再模拟控制台修改，最后确认恢复后不重复写入。
			cf.AddUpdateDomainRecords()
			if updates != 0 {
				t.Fatalf("matching record was updated %d times", updates)
			}
			remoteIP = tt.remoteIP
			cf.AddUpdateDomainRecords()
			if updates != 1 || remoteIP != tt.ipAddr || domain.UpdateStatus != config.UpdatedSuccess {
				t.Fatalf("remote drift not repaired: updates=%d, IP=%q, status=%s", updates, remoteIP, domain.UpdateStatus)
			}
			cf.AddUpdateDomainRecords()
			if zoneReads != 3 || recordReads != 3 || updates != 1 {
				t.Errorf("requests: zones=%d, records=%d, updates=%d; want 3, 3, 1", zoneReads, recordReads, updates)
			}
		})
	}
}

func TestCloudflareRecreatesDeletedRecordWithUnchangedIP(t *testing.T) {
	domain := &config.Domain{DomainName: "example.com", SubDomain: "home", CustomParams: "proxied=true&comment=managed"}
	cf := cloudflareTestProvider("A", "192.0.2.1", domain)
	recordExists, recordReads, creates := true, 0, 0
	cf.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/client/v4/zones":
			return cloudflareTestResponse(`{"success":true,"result":[{"id":"zone-id"}]}`), nil
		case request.Method == http.MethodGet && request.URL.Path == "/client/v4/zones/zone-id/dns_records":
			recordReads++
			if got := request.URL.Query().Get("comment"); got != "managed" {
				t.Errorf("comment filter = %q, want managed", got)
			}
			if !recordExists {
				return cloudflareTestResponse(`{"success":true,"result":[]}`), nil
			}
			return cloudflareTestResponse(`{"success":true,"result":[{"id":"record-id","content":"192.0.2.1","proxied":true}]}`), nil
		case request.Method == http.MethodPost && request.URL.Path == "/client/v4/zones/zone-id/dns_records":
			creates++
			var record CloudflareRecord
			if err := json.NewDecoder(request.Body).Decode(&record); err != nil {
				t.Fatalf("decode create: %v", err)
			}
			if record.Name != "home.example.com" || record.Type != "A" || record.Content != "192.0.2.1" || !record.Proxied || record.Comment != "managed" {
				t.Errorf("unexpected new record: %+v", record)
			}
			recordExists = true
			return cloudflareTestResponse(`{"success":true}`), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
			return nil, errors.New("unexpected request")
		}
	})}
	cf.AddUpdateDomainRecords()
	recordExists = false
	cf.AddUpdateDomainRecords()
	cf.AddUpdateDomainRecords()
	if recordReads != 3 || creates != 1 || !recordExists || domain.UpdateStatus != config.UpdatedSuccess {
		t.Errorf("record recovery: reads=%d, creates=%d, exists=%v, status=%s", recordReads, creates, recordExists, domain.UpdateStatus)
	}
}

func TestCloudflareSkipsRequestsWithoutLocalIP(t *testing.T) {
	for _, recordType := range []string{"A", "AAAA"} {
		t.Run(recordType, func(t *testing.T) {
			cf := cloudflareTestProvider(recordType, "", &config.Domain{DomainName: "example.com"})
			cf.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				t.Fatalf("request without local IP: %s %s", request.Method, request.URL)
				return nil, errors.New("unexpected request")
			})}
			cf.AddUpdateDomainRecords()
		})
	}
}

func TestCloudflareQueryFailureDoesNotBlockOtherDomains(t *testing.T) {
	for _, failure := range []string{"zone transport", "zone unsuccessful", "zone missing", "record transport", "record unsuccessful"} {
		t.Run(failure, func(t *testing.T) {
			badDomain := &config.Domain{DomainName: "bad.example.com"}
			goodDomain := &config.Domain{DomainName: "good.example.net"}
			cf := cloudflareTestProvider("A", "192.0.2.1", badDomain, goodDomain)
			goodQueries, creates := 0, 0
			cf.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/client/v4/zones":
					if request.URL.Query().Get("name") == badDomain.DomainName {
						switch failure {
						case "zone transport":
							return nil, errors.New("zone query unavailable")
						case "zone unsuccessful":
							// 即使错误响应带有 result，也不能继续查询或创建记录。
							return cloudflareTestResponse(`{"success":false,"messages":["denied"],"result":[{"id":"bad-zone"}]}`), nil
						case "zone missing":
							return cloudflareTestResponse(`{"success":true,"result":[]}`), nil
						}
						return cloudflareTestResponse(`{"success":true,"result":[{"id":"bad-zone"}]}`), nil
					}
					if request.URL.Query().Get("name") != goodDomain.DomainName {
						t.Fatalf("unexpected zone: %s", request.URL)
					}
					return cloudflareTestResponse(`{"success":true,"result":[{"id":"good-zone"}]}`), nil
				case request.Method == http.MethodGet && request.URL.Path == "/client/v4/zones/bad-zone/dns_records":
					if failure == "record transport" {
						return nil, errors.New("record query unavailable")
					}
					if failure == "record unsuccessful" {
						return cloudflareTestResponse(`{"success":false,"messages":["denied"],"result":[]}`), nil
					}
					t.Fatalf("queried records after zone failure: %s", failure)
					return nil, errors.New("unexpected request")
				case request.Method == http.MethodGet && request.URL.Path == "/client/v4/zones/good-zone/dns_records":
					goodQueries++
					return cloudflareTestResponse(`{"success":true,"result":[]}`), nil
				case request.Method == http.MethodPost && request.URL.Path == "/client/v4/zones/good-zone/dns_records":
					creates++
					return cloudflareTestResponse(`{"success":true}`), nil
				default:
					t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
					return nil, errors.New("unexpected request")
				}
			})}
			cf.AddUpdateDomainRecords()
			if badDomain.UpdateStatus != config.UpdatedFailed {
				t.Errorf("failed domain status = %s", badDomain.UpdateStatus)
			}
			if goodQueries != 1 || creates != 1 || goodDomain.UpdateStatus != config.UpdatedSuccess {
				t.Errorf("healthy domain blocked: queries=%d, creates=%d, status=%s", goodQueries, creates, goodDomain.UpdateStatus)
			}
		})
	}
}

func TestCloudflareRetriesFailedUpdateNextCycle(t *testing.T) {
	for _, failure := range []string{"transport", "unsuccessful"} {
		t.Run(failure, func(t *testing.T) {
			domain := &config.Domain{DomainName: "example.com"}
			cf := cloudflareTestProvider("A", "192.0.2.1", domain)
			reads, updates := 0, 0
			cf.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/client/v4/zones":
					return cloudflareTestResponse(`{"success":true,"result":[{"id":"zone-id"}]}`), nil
				case request.Method == http.MethodGet && request.URL.Path == "/client/v4/zones/zone-id/dns_records":
					reads++
					return cloudflareTestResponse(`{"success":true,"result":[{"id":"record-id","content":"192.0.2.2"}]}`), nil
				case request.Method == http.MethodPut && request.URL.Path == "/client/v4/zones/zone-id/dns_records/record-id":
					updates++
					if updates == 1 {
						if failure == "transport" {
							return nil, errors.New("update unavailable")
						}
						return cloudflareTestResponse(`{"success":false,"messages":["try again"]}`), nil
					}
					return cloudflareTestResponse(`{"success":true}`), nil
				default:
					t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
					return nil, errors.New("unexpected request")
				}
			})}
			cf.AddUpdateDomainRecords()
			if domain.UpdateStatus != config.UpdatedFailed {
				t.Fatalf("first update status = %s, want failure", domain.UpdateStatus)
			}
			cf.AddUpdateDomainRecords()
			if reads != 2 || updates != 2 || domain.UpdateStatus != config.UpdatedSuccess {
				t.Errorf("next cycle retry: reads=%d, updates=%d, status=%s", reads, updates, domain.UpdateStatus)
			}
		})
	}
}

func TestCloudflareModifyProxied(t *testing.T) {
	tests := []struct {
		name          string
		customParams  string
		recordIP      string
		ipAddr        string
		recordProxied bool
		wantRequest   bool
		wantProxied   bool
	}{
		{
			name:          "equivalent IPv6 representations do not trigger updates",
			recordIP:      "2001:db8::1",
			ipAddr:        "2001:0db8:0000:0000:0000:0000:0000:0001",
			recordProxied: true,
			wantRequest:   false,
		},
		{
			name:          "preserve proxy status without custom parameter",
			recordIP:      "192.0.2.1",
			ipAddr:        "192.0.2.1",
			recordProxied: true,
			wantRequest:   false,
		},
		{
			name:          "preserve proxy status when IP changes",
			recordIP:      "192.0.2.1",
			ipAddr:        "192.0.2.2",
			recordProxied: true,
			wantRequest:   true,
			wantProxied:   true,
		},
		{
			name:          "sync explicitly disabled proxy status without IP changes",
			customParams:  "proxied=false",
			recordIP:      "192.0.2.1",
			ipAddr:        "192.0.2.1",
			recordProxied: true,
			wantRequest:   true,
			wantProxied:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestCount := 0
			var updatedRecord CloudflareRecord
			client := &http.Client{
				Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					requestCount++
					if err := json.NewDecoder(request.Body).Decode(&updatedRecord); err != nil {
						t.Fatalf("decode request body: %v", err)
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{"success":true}`)),
						Header:     make(http.Header),
					}, nil
				}),
			}

			cf := Cloudflare{TTL: 1, httpClient: client}
			domain := &config.Domain{
				DomainName:   "example.com",
				SubDomain:    "test",
				CustomParams: tt.customParams,
			}
			cf.modify(CloudflareRecordsResp{
				Result: []CloudflareRecord{{
					ID:      "record-id",
					Content: tt.recordIP,
					Proxied: tt.recordProxied,
				}},
			}, "zone-id", domain, tt.ipAddr)

			if got := requestCount > 0; got != tt.wantRequest {
				t.Fatalf("request sent = %v, want %v", got, tt.wantRequest)
			}
			if tt.wantRequest && updatedRecord.Proxied != tt.wantProxied {
				t.Errorf("updated proxied = %v, want %v", updatedRecord.Proxied, tt.wantProxied)
			}
		})
	}
}
