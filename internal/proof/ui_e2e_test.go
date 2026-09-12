package proof

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"vigil/internal/browser"
)

// TestProofUIVisual uses a fresh isolated profile, never an operator's open tabs.
// Opt in with PROOF_UI_E2E_URL and PROOF_UI_E2E_DIR for the local QA deployment.
func TestProofUIVisual(t *testing.T) {
	raw := os.Getenv("PROOF_UI_E2E_URL")
	if raw == "" {
		t.Skip("set PROOF_UI_E2E_URL to opt into local browser QA")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.User != nil {
		t.Fatal("PROOF_UI_E2E_URL must be an explicit local HTTP URL")
	}
	dir := os.Getenv("PROOF_UI_E2E_DIR")
	if dir == "" {
		t.Fatal("PROOF_UI_E2E_DIR is required to preserve visual evidence")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	provider := browser.NewChromium("", true, dir)
	endpoint, err := provider.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Stop()
	alloc, cancelAlloc := chromedp.NewRemoteAllocator(ctx, endpoint.WebSocketURL, chromedp.NoModifyURL)
	defer cancelAlloc()
	page, cancelPage := chromedp.NewContext(alloc)
	defer cancelPage()
	for _, viewport := range []struct {
		name          string
		width, height int64
		dark          bool
	}{{"desktop", 1280, 900, false}, {"tablet", 768, 1024, false}, {"mobile", 390, 844, false}, {"mobile-dark", 390, 844, true}} {
		t.Run(viewport.name, func(t *testing.T) {
			var shot []byte
			var observation struct {
				Overflow bool `json:"overflow"`
				Buttons  []struct {
					Text   string  `json:"text"`
					Height float64 `json:"height"`
				} `json:"buttons"`
				Contrast []struct {
					Text  string  `json:"text"`
					Ratio float64 `json:"ratio"`
				} `json:"contrast"`
				URL string `json:"url"`
			}
			actions := []chromedp.Action{chromedp.EmulateViewport(viewport.width, viewport.height)}
			media := emulation.SetEmulatedMedia()
			if viewport.dark {
				media = media.WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: "dark"}})
			}
			actions = append(actions, media, chromedp.Navigate(raw),
				chromedp.WaitVisible("#app h1", chromedp.ByQuery),
				chromedp.Evaluate(`(() => {
     const rgb=s=>(s.match(/[\d.]+/g)||[]).map(Number);
     const lum=c=>c.slice(0,3).map(v=>v/255).map(v=>v<=.04045?v/12.92:Math.pow((v+.055)/1.055,2.4)).reduce((a,v,i)=>a+v*[.2126,.7152,.0722][i],0);
     const bg=e=>{for(let p=e;p;p=p.parentElement){const c=rgb(getComputedStyle(p).backgroundColor);if(c.length===3||c[3]>0)return c;}return [255,255,255];};
     const samples=[...document.querySelectorAll('h1,h2,p,.badge,button,dt')].filter(e=>e.getBoundingClientRect().height&&e.textContent.trim());
     return {url:location.href,overflow:document.documentElement.scrollWidth>innerWidth,
      buttons:[...document.querySelectorAll('button')].filter(e=>e.getBoundingClientRect().height).map(e=>({text:e.textContent,height:e.getBoundingClientRect().height})),
      contrast:samples.map(e=>{const a=lum(rgb(getComputedStyle(e).color)),b=lum(bg(e));return {text:e.textContent.slice(0,80),ratio:(Math.max(a,b)+.05)/(Math.min(a,b)+.05)};})};
    })()`, &observation),
				chromedp.FullScreenshot(&shot, 100))
			err := chromedp.Run(page, actions...)
			if err != nil {
				t.Fatal(err)
			}
			if observation.URL != raw {
				t.Fatalf("unexpected navigation: %s", observation.URL)
			}
			if err := os.WriteFile(filepath.Join(dir, viewport.name+".png"), shot, 0600); err != nil {
				t.Fatal(err)
			}
			body, _ := json.MarshalIndent(observation, "", "  ")
			if err := os.WriteFile(filepath.Join(dir, viewport.name+"-observations.json"), body, 0600); err != nil {
				t.Fatal(err)
			}
			if observation.Overflow {
				t.Error("page overflows viewport horizontally")
			}
			for _, b := range observation.Buttons {
				if b.Height < 44 {
					t.Errorf("button %q height %.1f is below 44px", b.Text, b.Height)
				}
			}
			for _, c := range observation.Contrast {
				if c.Ratio < 4.5 {
					t.Errorf("text %q contrast %.2f is below 4.5:1", c.Text, c.Ratio)
				}
			}
		})
	}
}
