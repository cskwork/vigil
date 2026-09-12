package proof

import (
	"context"
	"sync"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"vigil/internal/browser"
)

// observeForPlan visits one registered entry URL without credentials or writes.
// It records control structure only; observed page content never becomes an expected value.
func (s *Service) observeForPlan(parent context.Context, checkID string, target Target) PlanObservation {
	result := PlanObservation{URL: target.BaseURL, Status: "unavailable", ObservedAt: time.Now().UTC(), Note: "등록된 시작 화면을 읽지 못했습니다."}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	journalKey := "proof:browser:plan:" + checkID
	provider := browser.NewTrackedChromium("", true, "", func(pid int, profile string) error {
		return s.Store.SetState(context.Background(), journalKey, encode(ownedBrowser{PID: pid, Profile: profile}))
	})
	defer s.Store.DB().ExecContext(context.Background(), `DELETE FROM scheduler_state WHERE key=?`, journalKey)
	defer provider.Stop()
	endpoint, err := provider.Ensure(ctx)
	if err != nil {
		return result
	}
	alloc, stopAlloc := chromedp.NewRemoteAllocator(ctx, endpoint.WebSocketURL, chromedp.NoModifyURL)
	defer stopAlloc()
	page, stopPage := chromedp.NewContext(alloc)
	defer stopPage()
	blocked := false
	documents := map[string]bool{}
	var mu sync.Mutex
	chromedp.ListenTarget(page, func(event any) {
		if paused, ok := event.(*fetch.EventRequestPaused); ok {
			go func() {
				if paused.ResponseStatusCode != 0 {
					headers := append([]*fetch.HeaderEntry{}, paused.ResponseHeaders...)
					headers = append(headers, &fetch.HeaderEntry{Name: "Content-Security-Policy", Value: "sandbox allow-scripts allow-same-origin allow-forms; frame-src 'none'; object-src 'none'; worker-src 'none'"})
					_ = chromedp.Run(page, fetch.ContinueResponse(paused.RequestID).WithResponseCode(paused.ResponseStatusCode).WithResponseHeaders(headers))
					return
				}
				allowed := (paused.Request.Method == "GET" || paused.Request.Method == "HEAD" || paused.Request.Method == "OPTIONS") && Allowed(paused.Request.URL, paused.Request.Method, target.Scope)
				if allowed && paused.ResourceType == network.ResourceTypeDocument {
					mu.Lock()
					if !documents[paused.Request.URL] && len(documents) >= 2 {
						allowed = false
					} else {
						documents[paused.Request.URL] = true
					}
					mu.Unlock()
				}
				if allowed {
					_ = chromedp.Run(page, fetch.ContinueRequest(paused.RequestID))
				} else {
					mu.Lock()
					blocked = true
					mu.Unlock()
					_ = chromedp.Run(page, fetch.FailRequest(paused.RequestID, network.ErrorReasonBlockedByClient))
				}
			}()
		}
	})
	var controls []PlanControl
	err = chromedp.Run(page,
		network.Enable(),
		network.SetBypassServiceWorker(true),
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{
			{URLPattern: "*", RequestStage: fetch.RequestStageRequest},
			{URLPattern: "*", ResourceType: network.ResourceTypeDocument, RequestStage: fetch.RequestStageResponse},
		}),
		chromedp.ActionFunc(func(c context.Context) error {
			_, err := cdppage.AddScriptToEvaluateOnNewDocument(`Object.defineProperty(window,'open',{value:()=>null,writable:false});document.addEventListener('click',e=>{let a=e.target.closest?.('a');if(a&&a.target&&a.target!=='_self')e.preventDefault()},true);`).Do(c)
			return err
		}),
		chromedp.Navigate(target.BaseURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Evaluate(`(()=>[...document.querySelectorAll('button,a,input,textarea,select,[role],[data-testid]')].slice(0,50).map(e=>({tag:e.tagName.toLowerCase(),role:e.getAttribute('role')||'',type:e.getAttribute('type')||'',name:(e.getAttribute('aria-label')||e.getAttribute('placeholder')||e.getAttribute('name')||'').slice(0,120),test_id:(e.getAttribute('data-testid')||'').slice(0,120)})))()`, &controls),
	)
	if err != nil {
		return result
	}
	result.Controls = controls
	result.Status = "observed"
	result.Note = "등록된 시작 화면 1곳의 컨트롤 구조를 읽었습니다."
	mu.Lock()
	if blocked {
		result.Status = "partial"
		result.Note = "시작 화면은 읽었지만 허용되지 않은 네트워크 요청을 차단했습니다."
	}
	mu.Unlock()
	return result
}
