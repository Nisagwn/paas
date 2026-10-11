# paas — Tasarım Sistemi Kuralları (Figma → kod)

Bu belge, Figma tasarımlarını (Figma MCP: `get_design_context`, `get_screenshot`, `get_variable_defs`) bu kod tabanına aktarırken uyulacak kuralları tanımlar. Figma'dan gelen React + Tailwind çıktısı **referanstır**; nihai kod her zaman aşağıdaki yapıya çevrilir.

---

## 1. Teknoloji yığını (önce bunu bil)

| Konu | Kullanılan | Kullanılmayan |
|---|---|---|
| UI | Go `html/template`, sunucu tarafında render | React, Vue, JSX |
| Etkileşim | **htmx 2.0.4** (unpkg, SRI + nonce) + gerekirse nonce'lu küçük inline `<script>` | SPA framework'ü, npm paketleri |
| Canlı veri | htmx polling (`hx-trigger="every 3s"`), log'lar için `EventSource` | WebSocket istemci kütüphaneleri |
| Stil | Tek global `<style>` bloğu + CSS custom property token'ları | Tailwind, CSS Modules, styled-components, Sass |
| Build | **Yok.** Şablonlar `//go:embed templates/*.html` ile binary'ye gömülür | Vite/webpack/PostCSS |
| Dil | Arayüz metinleri **Türkçe** (`<html lang="tr">`) | — |

Kod, `internal/web/` paketindedir (`web.go:1` paket açıklaması). `examples/` altındaki Next.js/Vite projeleri platformda deploy edilen örnek uygulamalardır, **ürün arayüzü değildir** — tasarım işlerinde onlara dokunma.

**Kural:** Figma çıktısındaki Tailwind sınıflarını, React bileşenlerini veya yeni npm bağımlılıklarını asla doğrudan ekleme. Her şeyi mevcut sınıflara ve `var(--token)` değerlerine eşle.

---

## 2. Token tanımları

Tüm token'lar tek yerde: `internal/web/templates/layout.html` içindeki `<style>` bloğunun `:root` kuralı. Token dönüştürme sistemi (Style Dictionary vb.) yok; değerler elle yazılır.

```css
:root {
  --bg: #fafafa; --surface: #fff; --text: #171717; --muted: #6b6b6b; --border: #e5e5e5;
  --accent: #0a66ff; --accent-text: #fff; --code-bg: #0d0d0d; --code-text: #e6e6e6;
  --ok: #0f7b3d; --ok-bg: #e6f6ec; --err: #c62828; --err-bg: #fdecec;
  --run: #0a5bd6; --run-bg: #e8f0fe; --idle: #666; --idle-bg: #f0f0f0;
  --warn: #9a5b00; --warn-bg: #fff4e0; --chart-req: #2f6fde; --chart-err: #d03b3b; --grid: #ececec;
  --hover: #f4f4f5; --shadow: 0 6px 20px rgba(0,0,0,.08);
  --radius: 10px; --mono: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}
@media (prefers-color-scheme: dark) { :root { /* aynı adlar, koyu değerler */ } }
```

### Renk token'ları (anlamsal)

| Token | Anlam | Figma değişkeni eşlemesi |
|---|---|---|
| `--bg` / `--surface` | Sayfa zemini / kart zemini | background / surface |
| `--text` / `--muted` | Birincil / ikincil metin | text-primary / text-secondary |
| `--border` | Tüm çizgi ve kenarlıklar | border-default |
| `--accent` / `--accent-text` | Link, birincil buton, focus halkası | primary / on-primary |
| `--ok`, `--ok-bg` | Başarılı, Hazır, Canlı | success |
| `--err`, `--err-bg` | Hata, Başarısız, tehlikeli eylem | danger |
| `--run`, `--run-bg` | Devam eden (build/deploy), Önizleme | info / in-progress |
| `--warn`, `--warn-bg` | Uyarı, Bozulmuş sağlık | warning |
| `--idle`, `--idle-bg` | Pasif, İptal, trafik yok | neutral |
| `--code-bg`, `--code-text` | Log panelleri (temadan bağımsız koyu) | code |
| `--chart-req`, `--chart-err`, `--grid` | SVG grafik serileri ve ızgara | chart |
| `--hover` | Menü öğesi / üst menü bağlantısı üzerine gelince zemin | surface-hover |
| `--shadow` | Açılır menü gölgesi (koyu temada daha koyu) | elevation |

### Diğer ölçüler (token değil, sabit — tutarlı kullan)

- **Tipografi:** `body` = `15px/1.5 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif`. Web fontu **yüklenmez**.
  - `h1` 24px (hero 28px), `h2` 16px, `.small` 13px, tablo başlığı/etiket 12px büyük harf `letter-spacing:.04em`, mono 13px (`var(--mono)`), istatistik değeri 26px `tabular-nums`.
  - Başlıklarda negatif harf aralığı: `h1 -.02em`, `.brand -.01em`.
- **Boşluk ölçeği:** 4 · 6 · 8 · 10 · 12 · 14 · 16 · 20 · 24 · 32 · 48 · 64 px. Kart iç boşluğu 16px (`.pad`), grid/row aralığı 12px, bölüm başlığı üst boşluğu 32px.
- **Köşe yarıçapı:** kart `var(--radius)` (10px), buton/input/menü 8px, `.tag` 6px, `.badge` 999px (hap).
- **Gölge:** yalnızca açılır menüde `var(--shadow)`. Kartlar gölgesiz; ayrım kenarlıkla yapılır.
- **Maks. genişlik:** `main` 1080px (yorum/review sayfası 1600px'e genişletir).

### Figma değişkenlerini aktarma kuralları

1. `get_variable_defs` ile gelen her renk için önce yukarıdaki tablodan **mevcut** token'ı bul; hex değerini asla şablona gömme.
2. Gerçekten yeni bir anlam gerekiyorsa token'ı `:root`'a **ve** `prefers-color-scheme: dark` bloğuna birlikte ekle. Koyu değer verilmemiş token eklemek yasak.
3. Figma'daki 4px'e yakın değerleri yukarıdaki boşluk ölçeğine yuvarla (ör. 15px → 16px).

---

## 3. Bileşen kütüphanesi

Bileşenler iki katmandan oluşur; Storybook veya ayrı bir doküman sitesi yoktur.

### 3.1 CSS bileşen sınıfları (`layout.html`)

| Sınıf | Kullanım |
|---|---|
| `.card`, `.card.pad` | Kenarlıklı yüzey; içerik bloklarının tamamı bunun içinde |
| `.head` + `.head .actions` | Başlık solda, eylemler `margin-left:auto` ile sağda |
| `.row` (+ `> div`) | Form satırı; esnek, sarılır, alanlar min 160px. Buton sütunu için `style="flex:0"` |
| `.grid.kv` (`.k` etiket) | Anahtar–değer bilgi ızgarası |
| `.stats` / `.stat .k .v` | Metrik kutucukları |
| `.projects` / `.project` | Proje kartı ızgarası (min 300px) |
| `.badge.s-{status}` | Durum hapı, başında renkli nokta; `s-building`/`s-deploying` nokta yanıp söner |
| `.h-{health}` | Sağlık rozeti (`healthy`, `degraded`, `failing`, `no_traffic`) |
| `.tag`, `.tag.prod`, `.tag.warn`, `.tag.env-production`, `.tag.env-preview` | Küçük çerçeveli etiket |
| `button`, `.btn` + `.primary` `.danger` `.small` `.big` `.link` | Butonlar (link görünümlü buton için `.btn`) |
| `.flash.ok` / `.flash.err` | Bildirim kutusu (`role="status"` / `role="alert"`) |
| `nav.tabs` (`a.on` + `aria-current="page"`) | Sayfa sekmeleri |
| `nav.seg` (`a.on`) | Segmentli seçici (ör. zaman aralığı) |
| `details.section` (`> summary`, `> .body`) | Açılır bölüm, `›` oku döner |
| `.empty`, `.hero` | Boş durum / ilk kullanım ekranı |
| `.log` (`.step`, `.bad`, `.meta`) | Koyu log paneli |
| `.mono`, `.muted`, `.small`, `.trunc`, `.hide-sm` | Yardımcı sınıflar |
| `svg.chart` (`.bar`, `.bar-err`, `.grid`, `.axis`, `.tick`) | Sunucuda üretilen grafik |
| `header.top`, `nav.primary` (`a.on` + `aria-current="page"`), `.top-actions`, `.top-sep`, `.top-team` | Üst menü (yalnızca `layout.html`); bölüm vurgusu `page.Nav.Section`'dan gelir |
| `details.dropdown` (`> summary`, `> .menu`, `.menu-right`, `.menu-head`, `.check`, `hr`) | Açılır menü (ekip seçici, hesap menüsü); dışarı tıklama ve Escape kapatır. `.user-menu` eski adıyla çalışır |
| `.avatar`, `.avatar.square` | Baş harf rozeti: yuvarlak (kullanıcı) / köşeli (proje, repo). Görsel URL'si CSP yüzünden yüklenmez |
| `nav.crumbs` | Konum çizgisi (Ekip / Proje / Deploy). Elle yazma, `{{crumbs …}}` fonksiyonunu kullan (§3.4) |
| `button.copy` | Panoya kopyala butonu; `{{copy .URL}}` üretir. JS yoksa gizlidir, metin seçilebilir kalır |
| `.js-only` | Yalnızca JS varken görünür (`html.js`); ör. `data-back` butonu, `kbd` ipucu |
| `.sr-only` | Görsel olarak gizli, ekran okuyucuya açık metin (ör. arama `label`'ı) |
| `.skip` | "İçeriğe geç" bağlantısı (layout'ta; `main#main`'e gider) |
| `kbd` | Klavye kısayolu ipucu (ör. arama kutusunda `/`) |
| `.filterbar` (`.search`, `.count`) | Liste üstü arama satırı: `input[type=search][data-search][data-filter="#liste"]` + sayaç. `?q=` ile JS'siz de filtrelenir |
| `.project` (`a.stretch`, `.p-head`, `.p-line`, `.p-line .grow`, `.p-foot`), `ul.projects` | Proje kartı: `a.stretch` tüm kartı tıklanır yapar; kart içindeki diğer bağlantı/butonlar üstte kalır |
| `ol.steps` (`li.on` + `aria-current="step"`, `li.done`, `.n`, `.label`) | Adım göstergesi (Yeni proje akışı); 720px altında yalnızca etkin adımın adı görünür |
| `ol.checklist` (`li.done/.current/.todo/.skip`, `.n`, `.grow`) | İlk kullanım kontrol listesi; `.current` solda vurgu çizgisi alır |
| `dl.facts` (`dt`, `dd`) | Etiket–açıklama listesi ("Ne olacak?" paneli); mobilde tek sütun |
| `.flash .help` | Hata kutusunun altında "ne yapmalı" satırı + bağlantı |
| `.error-page` (`.code`, `.actions`) | Hata / yönlendirme kartı |
| `.auth-card`, `.btn.block`, `.divider` | Giriş kartı, tam genişlik buton, "ya da" ayırıcı |
| `.btn.small`, `.btn.danger`, `button:disabled` | Bağlantı-buton küçük/tehlikeli varyantı; devre dışı görünüm |

### 3.2 Şablon parçaları (`{{define}}` / `{{template}}`)

- `templates/layout.html` → `"layout"`: iskelet, üst menü, flash mesajları; sayfa `"content"` (ve isteğe bağlı `"head"`) bloğunu tanımlar. Hatayı kendi kartında gösteren sayfa `"flash"` bloğunu yeniden tanımlar (boş gövde Go'da yok sayılır; bkz. `login.html`, `error.html`).
- `templates/partials.html` → `apphead` (proje başlığı + sekmeler), `depbadges`, `depcommit`, `recent`, `deployments`, `env`, `domains`.
- `templates/processes.html`, `templates/rollout.html` → `processes`, `rollout` parçaları.
- Parçalar hem tam sayfada hem htmx yanıtında kullanılır; her ikisi de `.Data` ve `.CSRF` taşıyan bir değer alır.

```gotemplate
{{/* depbadges: status, production, sleeping, environment of one row. */}}
{{define "depbadges"}}
<a href="/deployments/{{.ID}}"><span class="badge s-{{.Status}}">{{status .Status}}</span></a>
{{if .Production}}<span class="tag prod">Canlı</span>{{end}}
{{end}}
```

### 3.3 Yeni sayfa / parça ekleme

1. `internal/web/templates/<ad>.html` içinde `{{define "content"}}…{{end}}` yaz.
2. `internal/web/web.go` → `Handler()` içindeki sayfa listesine `<ad>` ekle (her sayfa `layout`, `partials`, `processes`, `rollout` ile birlikte parse edilir).
3. htmx ile tek başına dönen bir parça ise `s.pages["<ad>"] = partials.Lookup("<ad>")` satırını ekle ve `s.partial(...)` ile render et.
4. Handler'da `s.render(w, r, status, "<ad>", "Başlık", data, errMsg)` çağır.
5. `internal/web/*_test.go` içinde HTML'de beklenen metni/eylemi doğrulayan test ekle (bkz. `features_ui_test.go`).

### 3.4 Şablon fonksiyonları (`web.go` `funcs`)

Görüntülenen her değer bunlardan geçmeli — Figma'daki örnek metni kopyalama:

| Fonksiyon | Çıktı |
|---|---|
| `status`, `role`, `kind`, `target`, `origin`, `health`, `envtarget` | İngilizce kimlik → Türkçe etiket (`tr.go`) |
| `tr` | Diğer paketlerden gelen İngilizce mesajı Türkçeleştirir |
| `ago`, `time`, `duration` | Göreli zaman, `02.01.2006 15:04:05 UTC`, süre |
| `num`, `pct`, `ms`, `bytes`, `cpu` | Türk biçimi: `12.345`, `%1,5`, `1,5 sn`, `120 MiB`, `1,5 çekirdek` (`format.go`) |
| `short`, `firstLine` | Kısa SHA, commit mesajının ilk satırı |
| `when` | `<time datetime title="tam zaman" data-ago>2 sa önce</time>`; `time.Time` ya da `*time.Time` alır (nil → `—`). Metin sayfada kendiliğinden güncellenir. Göreli zaman gösterirken `ago` yerine bunu kullan |
| `crumbs` | `{{crumbs "Ekip" "/teams/web" "blog" "/apps/blog" "Deploy #12"}}` → `nav.crumbs`; son etiket (href'siz) geçerli sayfadır. Tüm şablon setlerinde (htmx parçaları dahil) çalışır |
| `copy` | `{{copy .URL}}` ya da `{{copy .URL "Adresi kopyala"}}` → ikonlu `button.copy` |
| `framework` | Framework kimliği → görünen ad (`nextjs` → `Next.js`) |
| `join`, `add1`, `initialOf`, `trimScheme` | `strings.Join`; sayaç +1; baş harf; `https://` olmadan adres |

---

## 4. Stil yaklaşımı

- **Metodoloji:** global, anlamsal sınıf adları (`.card`, `.badge`) + durum değiştiricileri (`.s-ready`, `.on`, `.primary`). BEM, utility-first veya scoped CSS yok.
- **Yeni ortak stil** → `layout.html` `<style>` bloğuna, ilgili grubun yanına (dosyada `/* Faz 16–19 screens … */` gibi gruplama yorumları var).
- **Tek sayfaya özel stil** → o sayfanın `{{define "head"}}<style>…</style>{{end}}` bloğuna (global `<style>`'dan sonra gelir, yani aynı özgüllükte sayfa kuralı kazanır) (örnek: `templates/review.html:1`). Yine yalnızca `var(--…)` kullan.
- **Inline `style="…"`**: mevcut kodda yalnızca tek seferlik yerleşim ayarı için (margin, `flex:0`, `grid-column:1/-1`) kullanılır. Renk gerekiyorsa `style="color:var(--err)"` gibi **token** ile. Tekrar eden inline stil görürsen sınıfa çevir.
- **Koyu tema:** otomatik, `prefers-color-scheme` ile. Manuel tema anahtarı yok. Yeni renk = iki tema değeri.
- **Responsive:** tek kırılım `@media (max-width: 720px)` → `.hide-sm` gizlenir, tablo hücre boşluğu 8px olur. Izgaralar `repeat(auto-fit, minmax(…, 1fr))` ile, satırlar `flex-wrap: wrap` ile kendiliğinden uyum sağlar. Sayfaya özel ek kırılım gerekirse o sayfanın `head` bloğuna (review: 1000px).
- **Hareket:** yalnızca `@keyframes pulse` (devam eden durum noktası) ve `details` okunun `transform .15s` dönüşü. Figma'daki karmaşık animasyonları CSS geçişleriyle ve ölçülü uygula.

---

## 5. Varlıklar (asset) ve ikonlar

- **Görsel dosyası yok.** `static/` dizini, CDN, görsel optimizasyon hattı yoktur. Dış kaynak olarak yalnızca htmx unpkg'den yüklenir.
- **Favicon:** `layout.html` içinde data-URI SVG üçgen.
- **CSP** (`web.go` `securityHeaders`): `default-src 'self'; script-src 'nonce-…' https://unpkg.com; style-src 'self' 'unsafe-inline'; img-src 'self' data:`.
  - Her `<script>` etiketi `nonce="{{.Nonce}}"` taşımalı; `onclick=` gibi inline handler **çalışmaz**.
  - Dış görsel/font URL'si yüklenmez. Figma'dan görsel gerekiyorsa: küçük SVG'yi inline göm, raster görseli `data:` URI yap ya da `//go:embed` ile sunan bir handler ekle (CSP'yi buna göre güncelle). Figma MCP'nin localhost asset URL'lerini koda yazma.
- **İkonlar:** ikon kütüphanesi yok; **Unicode karakterler** kullanılır:
  - `▲` marka, `↗` dış bağlantı (`target="_blank" rel="noopener"` ile), `→` "devamı", `▾` açılır menü, `›` details oku (CSS `::before`), `—` boş değer, `•` gizli değer (`••••••••`), `…` yükleniyor.
  - Durum göstergesi ikon değil, `.badge::before` renkli noktasıdır.
  - Figma'da yeni ikon varsa: önce uygun bir Unicode karakter ara; yoksa `fill="currentColor"` kullanan, `aria-hidden="true"` inline SVG olarak ekle. İkon fontu veya sprite ekleme.
- **Grafikler:** istemci tarafı grafik kütüphanesi yok. SVG sunucuda üretilir (`internal/web/analytics.go` `chartSVG`, viewBox 720×200) ve `svg.chart` CSS sınıflarıyla token'lara bağlanır.

---

## 6. Etkileşim desenleri (Figma prototip davranışlarını böyle uygula)

- Her durum değiştiren eylem bir `<form method="post">`'tur ve `<input type="hidden" name="csrf" value="{{$csrf}}">` içerir; JS olmadan da çalışmalı. htmx üstüne eklenir:
  ```html
  <form class="inline" method="post" action="/apps/{{$app}}/cancel"
    hx-post="/apps/{{$app}}/cancel" hx-target="main" hx-select="main" hx-swap="outerHTML"
    hx-confirm="{{short .CommitSHA}} deploy'u iptal edilsin mi?">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <button class="small danger" type="submit">İptal et</button>
  </form>
  ```
- Onay diyalogları: `hx-confirm` (özel modal bileşeni yok). Figma'daki onay modalını buna eşle.
- Kart içi güncelleme: kartın kendi `id`'si + `hx-target="#env-card" hx-swap="outerHTML"`.
- Canlı yenileme: yalnızca bir iş sürerken `hx-trigger="every 3s"` ekle.
- Gecikmeli bölüm: `hx-get=… hx-trigger="load"` ve içinde `.card.pad.muted.small` "… yükleniyor…" yer tutucusu.
- Açılır menü / akordeon: `<details>`; JS yazma. Üst menü tarzı açılır menü için `details.dropdown`.
- **Global yardımcı script** (`layout.html` sonunda, nonce'lu). Hepsi aşamalı iyileştirmedir; JS olmadan sayfa çalışır. Yeni sayfada aynı işi yapan ikinci script yazma, bu `data-` özniteliklerini kullan:
  - `data-copy="metin"` → tıklayınca panoya kopyalar (`{{copy …}}` üretir), ekran okuyucuya "Kopyalandı." der.
  - `data-search` → `/` tuşu bu kutuya odaklanır (sayfada bir tane olsun).
  - `data-filter="#liste"` → `#liste` içindeki `[data-filter-text]` öğelerini canlı filtreler; boş kalan `[data-filter-group]` gizlenir; hiç sonuç yoksa `#liste-empty` görünür. Aynı form `GET ?q=` ile sunucuda da filtrelemeli.
  - `data-mirror="#hedef"` (+ `data-mirror-empty`) → input değerini hedef metne yansıtır (ör. proje adı → önizlenen adres).
  - `data-back` → tarayıcı geçmişinde geri gider (geçmiş yoksa `/`).
  - `time[data-ago]` → 30 sn'de bir göreli metni yeniler.
  - Her `method="post"` form gönderilince butonları devre dışı kalır, basılan butonun sonuna `…` eklenir, form `aria-busy="true"` olur (htmx formlarında istek bitince geri açılır). İstemiyorsan forma `data-no-busy` ekle.
- Ekip seçici seçimi `?team=<slug>` ile ana sayfada yapılır ve `paas_team` çerezinde saklanır; `page.Nav.Team` seçili ekiptir (nil: tüm ekipler).

---

## 7. İçerik ve erişilebilirlik kuralları

- Tüm görünen metin Türkçe, ikinci tekil şahıs, kısa ("Siteyi aç ↗", "Henüz deploy yok."). Teknik terimler (deploy, branch, production, commit) olduğu gibi kalır.
- Saklanan değerler İngilizce kimlikle kalır; etiketi `tr.go` haritalarına ekle.
- Ürünü küçülten kelimeler kullanma (mini, basit, MVP, küçük vb.) — ne metinde ne sınıf adında.
- Her `input`'un `<label for>`'u olsun; sekmelerde `aria-current="page"`, grafikte `role="img" aria-label`, `nav`'larda `aria-label`.
- Boş durumlar `.empty` ile ve bir sonraki adımı söyleyerek yazılır.
- Hata mesajının yanında ne yapılacağını söyle (`.flash .help` + bağlantı; ör. plan limiti → "Kullanımı gör").
- Klavye: tüm etkileşimli öğelerde `:focus-visible` halkası global gelir; `outline: none` yazma. Yeni sekmede açılan bağlantıya `↗` ve `<span class="sr-only">(yeni sekmede açılır)</span>` ekle.

---

## 8. Proje yapısı

```
minipaas/
├── cmd/                     # binary'ler (paas, paas-cli, paas-dockerfile, paas-envkey)
├── internal/
│   ├── web/                 # ← TÜM ARAYÜZ BURADA
│   │   ├── web.go           # Server, funcs, route'lar, render/partial, CSP
│   │   ├── tr.go            # Türkçe etiket haritaları + mesaj çevirisi
│   │   ├── format.go        # sayı/süre/bayt biçimleri
│   │   ├── <özellik>.go     # app, analytics, databases, processes, rollouts, comments, github…
│   │   ├── *_test.go        # HTML çıktı testleri
│   │   └── templates/
│   │       ├── layout.html  # iskelet + TÜM token'lar ve global CSS
│   │       ├── partials.html, processes.html, rollout.html   # ortak parçalar
│   │       └── <sayfa>.html # her biri {{define "content"}}
│   ├── api/ store/ build/ deploy/ …   # arka uç (JSON API İngilizce)
├── docs/  infra/  loadtest/
└── examples/                # deploy edilen örnek uygulamalar (UI değil)
```

Özellik bazlı düzen: bir ekran = `internal/web/<özellik>.go` (handler + view modeli) + `templates/<özellik>.html` + `<özellik>_test.go`.

---

## 9. Figma → kod iş akışı

1. `get_design_context` + `get_screenshot` ile tasarımı al; `get_variable_defs` ile değişkenleri oku.
2. Her görsel öğeyi §3'teki sınıflara eşle; yoksa önce mevcut sınıfları birleştirmeyi dene, son çare yeni sınıf.
3. Renk/ölçüleri §2 token ve ölçeklerine çevir; iki temada da kontrol et.
4. Metni Türkçe yaz, değerleri şablon fonksiyonlarından geçir.
5. Eylemleri §6 form + htmx desenine çevir (CSRF zorunlu, JS'siz çalışmalı).
6. `go test ./internal/web/...` çalıştır; ekranı açıp ekran görüntüsünü Figma ile karşılaştır (açık + koyu tema, 720px altı).

Code Connect eşlemesi yapılacaksa, React bileşeni yerine `templates/partials.html` içindeki `{{define}}` bloğunu ve kullanılan CSS sınıfını referans göster.
