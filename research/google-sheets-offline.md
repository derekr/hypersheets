# How Google Sheets / Docs offline actually works

Research date: **2026-09-02**. Everything below was fetched on this date. Google's offline story
has been rebuilt three times since 2007, so every claim carries a date where the source has one.

**Source tiers:**

- **T1** — Google's own support docs, Workspace admin docs, Google product/engineering blogs,
  Chrome Web Store listing.
- **T2** — Google engineering write-ups on Google developer surfaces (web.dev case studies),
  conference talks, credible independent reverse-engineering.
- **T3** — dated user reports and reputable press. Used only for behaviour that is real but undocumented.

**A warning specific to this topic.** It is unusually thick with AI-generated fabrication.
Searches for "Google Docs offline conflict resolution" reliably surface confident, well-formatted
articles describing a Cassandra-backed op log and a "side-by-side diff" conflict-resolution UI in
Google Docs. **No such UI exists, and no Google source describes that storage.** Nothing of that
kind is cited here. Where I could not find a real source, the claim is in *What I could not
establish* rather than resolved.

**Specific claims that circulate and are false — do not propagate:**

- **"Google creates a '(Conflicted copy)' file for Docs offline conflicts."** Fabricated. Google
  uses the term "conflicted copy" nowhere on `support.google.com` for any product. It is Dropbox
  behaviour misattributed.
- **"Google Docs uses a differential update algorithm."** False — Google's own 2010 engineering
  series says Docs *abandoned* diffing for OT.
- **"Google Docs offline uses CRDTs."** False. Every credible source says OT.
- **US11126792B2 "Version history for offline edits" is assigned to Dropbox, not Google.**
  US8352870B2 "Conflict resolution" is Microsoft's. Both are miscited as Google's in search
  summaries.

Companion: `research/offline-scoping-patterns.md` covers the cross-product comparison. This is the
Google-specific deep dive.

---

## The answer to the scoping question

**Offline availability is a hybrid: an automatic recency cache you do not control, plus a manual
per-file pin you do.** Turning on the Drive **Offline setting** — one switch, per Google account,
per browser profile — makes Google automatically sync "a certain number" of your Docs/Sheets/Slides
"based on how recently you accessed them" (Google Workspace Updates, 2019-04-24, T1). Google's own
Chrome Web Store listing puts the only number anywhere on it: **"Hundreds of your most relevant
documents are automatically made available for offline viewing and editing"** (T1, listing fetched
2026-09-02, extension v1.109.1, updated 2026-08-20). On top of that you can **pin** specific files
via `File → Make available offline`, and those "always remain available offline".

**Who decided: mostly Google, then you, then your admin.** The automatic layer is the floor and it
has *no user-facing controls at all* — no count, no size budget, no "sync the last N days" dial.
That absence is a choice, not an oversight: Gmail offline, on the same platform, does ask you "how
many days of messages you want to sync" (Chromebook Help, T1). Docs deliberately does not ask. The
manual pin is the only user lever and it only ever adds. The Workspace admin sits above both and
can switch the whole feature off, or gate it to machines carrying a pushed device policy.

**Scope is per-account-per-browser-profile, and the unit is the file.** Google documents the
exclusivity in as many words: *"Another user has already enabled offline access on this computer.
Only one account for each browser profile can have offline enabled"* (T1). There is no folder-level
"available offline" for Docs/Sheets/Slides on the web — folder-level marking exists only in Drive
for desktop, and explicitly **not** for Google-format files.

**Ownership makes no documented difference.** Recency and pinning are the only stated criteria;
neither Google's user docs nor its admin docs mention ownership, sharing, or shared drives as a
factor. I could not positively confirm shared-drive behaviour either way — see the gaps list.

**If you go offline and open a sheet that was never cached, you cannot open it.** Drive's **Offline
preview** mode exists precisely to answer this *in advance*: it greys out every file that will not
be accessible (Google, 2019-04-24, T1; 9to5Google, 2019-04-24, T3). Google documents the
before-the-fact inspection and never documents the after-the-fact failure.

**One thing the scoping question does not ask but you should know before treating this as a model
to copy: the feature is not reliable, and Google knows.** Google's official forum surfaced roughly
23 distinct offline-data-loss threads in 2026 alone; the extension is rated 2.4/5 across 8.9K
ratings; Google's own volunteer Product Experts answer with a canned warning listing three
data-loss conditions that appear nowhere in Google's documentation, one of whom writes *"I would
never recommend it to anyone."* Details in §4.

**And the one that matters most here: this is not a fork.** You are editing *the file*, at its own
URL, with its own ID. The document status indicator changes to say the file is saved *to the
device* rather than *to the cloud* (Google, 2020-06-08, T1), and that is the entire extent of the
identity signal. No copy, no branch, no divergent ID, no reconciliation step. Google's model is a
**cache plus an outbox over one canonical document**, merged by Operational Transformation on
reconnect.

---

## 1. Mechanism

### Timeline

| Date | Event | Source |
|---|---|---|
| 2007–2010 | Offline Docs via the **Google Gears** browser extension. | Contemporary reporting (T3) |
| **2010-04-12** | New HTML5-based Docs editors ship as "previews" without Gears offline. | The Register, 2010-04-14 (T3) |
| **2010-05-03** | **Google removes offline support from Docs entirely.** Google's own words, quoted in press: *"we need to temporarily remove offline support for Docs starting May 3rd, 2010… we are working hard to bring a new and improved HTML5-based offline option back to Google Docs."* | The Register, 2010-04-14, quoting a Google blog post (T3 quoting T1) |
| 2011-08-31 | HTML5 Chrome apps bring **read-only** offline back to Docs. | TechCrunch, 2011-08-31 (T3) |
| **2012-06-30** | **Offline *editing* returns — for Docs only.** *"Users now have the ability to edit documents and leave comments offline. You must be running the latest version of Chrome or ChromeOS and install the Google Drive Chrome app."* | Workspace Updates, 2012-06-30 (T1) |
| **2013-01-23** | **Slides offline** — *"automatically enabled for users who have already **enabled offline editing of Docs and Sheets**."* Chrome / ChromeOS only. | Workspace Updates (T1) |
| **2013-12-11** | **New Google Sheets: "faster, more powerful, and works offline"** — *"just like Google Docs and Slides, you can now make edits to Sheets offline."* Default for everyone 2014-03-21. | blog.google / Workspace Updates (T1) |
| **2013** | **The Sheets calculation engine moves from the server into the browser.** Not coincidental — see §2. | web.dev case study, 2024-06-26 (T2, Google-authored) |
| **2015-02-03 → 2015-02-25** | **The "built into Chrome" attempt, announced and pulled in 22 days.** *"you'll be able to just sign into Chrome on the web and visit Drive, Docs, Sheets, or Slides—and offline will be enabled automatically. **This is already the default behavior on Chrome OS**."* Then: *"**Update to initial post (Feb 25): This feature launch is currently on hold.**"* | Workspace Updates (T1) |
| **2015-06-26** | Instead of auto-enable, the **extension is added to Chrome's default-install list**, gated to Google-branded Chrome. Users stop having to install anything; Google's help pages stop mentioning an extension (verified absent 2016-04 → 2018-08). | Chromium source (T2) |
| 2016-04-18 | Users can choose **which** files are available offline. | Workspace Updates (T1) |
| **2018-08-28 → 2018-11-28** | **The docs reverse**: the help page starts saying *"Install and enable Google Docs offline Chrome extension"* again. No announcement accompanies the change. | Help-page captures (T2) |
| **2017-04-24** | Admin console gains offline controls: *"Allow users to enable offline access"* vs *"Control offline access using device policies"*. Introduces the trusted-computer prompt: *"the user will be asked if the computer is a trusted one and warned not to turn on the setting for any public or shared device."* | Workspace Updates, 2017-04-24 (T1) |
| **2019-04-24** | Offline extends to the Drive web UI; **Offline preview mode** ships. States the scoping rule (see above) and that *"The Google Docs Offline extension, which is made available by default to all Chrome users, is still required."* | Workspace Updates, 2019-04-24 (T1) |
| **2020-06-08** | New document-status indicator: *"More descriptive text to indicate whether a document is saved to the cloud (when online) or to the device (offline)."* Interface only — *"there are no changes in the underlying functionality."* | Workspace Updates, 2020-06-08 (T1) |
| **2021-09-02** | Non-Google file types (PDF, images, Office) can be marked offline on Drive web, GA. ChromeOS Files app can mark Docs/Sheets/Slides offline. | Workspace Updates, 2021-09-02 (T1) |
| 2023-07 | **Microsoft Edge begins force-installing the Google Docs Offline extension** (disabled until you visit Docs). | Neowin / gHacks, July 2023 (T3) |
| **2024-06-26** | **Sheets calculation doubles in speed by moving the calc engine to WasmGC**, Chrome and Edge only. | Workspace blog + web.dev case study, both 2024-06-26 (T1/T2) |
| **2022-06-27** | *"Offline syncing available for opened Microsoft Office documents"* — **the last Workspace Updates post about browser offline, full stop.** | Workspace Updates (T1) |
| 2022-06-27 → 2026-09-02 | **Four years with no Workspace Updates announcement about browser offline**, while Gemini features ship weekly. Established by full-text search of the blog's own Blogger feed (72 posts matching "offline", 2009–2026), not a slug scan. | Negative finding (T1 feed) |

Two gaps worth naming, because they are the real story of this feature: **26 months with no offline
editing in Docs at all (2010-05 → 2012-06), and 43 months for Sheets (2010-05 → 2013-12).** Google
shipped a collaborative editor, removed offline entirely for over two years, and only brought
spreadsheets back once it had rewritten the calculation engine to run in the browser.

### What runs today (2026-09-02)

From Google's live help page, fetched today (T1):

> You must use the Google Chrome or Microsoft Edge browser. Don't use private browsing. Install and
> turn on Google Docs Offline Chrome extension. Make sure you have enough available space on your
> device to save your files.

- **The extension is required, and "built into Chrome" was never true as packaging.** This is worth
  stating precisely because the record looks like a flip-flop and is not. The extension has never
  been compiled into the Chrome binary. From **2015-06-26** it was added to Chrome's
  default-install list — downloaded from the Web Store on profile creation, listed in
  `chrome://extensions`, uninstallable, and gated to Google-branded Chrome builds (so Chromium
  users never got it). That produced a *user experience* of "built in" from mid-2015 to late 2018,
  during which Google's help pages stopped mentioning an extension at all. Between **2018-08-28 and
  2018-11-28** the help pages reverted to "install and enable", with no announcement. Google then
  stated both halves in one 2019 sentence: *"made available by default to all Chrome users, is
  **still required**."* As of 2026 it is still preinstalled in branded Chrome only — and since
  **2026-06-04** that preinstall extends to desktop Android via a hard-coded ID, with explicit
  logic not to re-add it after a user uninstalls (T2, Chromium source).
- **The extension today.** Chrome Web Store, fetched 2026-09-02: ID
  `ghbmnnjooekpmoecnnnilnnbdlolhkhi`, **v1.109.1, updated 2026-08-20**, 155 KiB, published by
  Google Ireland Ltd, **rated 2.4 / 5 from 8.9K ratings**. It does double duty as the cross-editor
  copy/paste bridge — *"This extension is also used to make advanced copy & paste functionality
  available in Google Docs, Sheets and Slides"* — which is why many people have it without wanting
  offline, and why uninstalling it has a second, unrelated cost.
- **Edge support arrived between 2022-04-07 and 2022-04-26** (help-page captures, T2), roughly a
  year before Microsoft began force-installing the extension in Edge.
- **Chrome or Edge only, and this is a shipping-vehicle limit rather than a capability limit
  today.** WasmGC — the thing Google cited for the Chrome/Edge-only calc improvement in 2024 — has
  been Baseline across Chrome 119, Firefox 120 and Safari 18.2 since December 2024 (T3, corroborated
  across several 2025–26 surveys). Google's docs still say Chrome/Edge. The binding constraint is a
  Chrome-Web-Store extension, which Firefox and Safari cannot install.
- **No incognito.** Documented, no reason given.
- **Per browser profile, one account.** Documented (T1, quoted above).
- **Auto-enabled in three cases**, per Google: *"Offline will automatically be enabled if you are a
  ChromeOS user, if you choose to sync your Docs and Drive when creating a Chrome Profile, or if you
  have Drive for desktop installed on your device."* (T1)
- **Drive for desktop does not provide offline Docs/Sheets/Slides.** Google is explicit: *"To make
  Google Docs, Sheets, and Slides available offline, use files offline with Drive on the web."*
  `.gdoc`/`.gsheet` files on disk are pointer files that open a browser. The filesystem route
  cannot do it. (T1)
- **Admin surface (knowledge.workspace.google.com, last updated 2026-08-26, T1):** default is on —
  *"By default, offline access is turned on for organizations, and users can turn it on or off for
  their own accounts."* Option 1: *"Allow users to enable offline access (recommended). Recent files
  are synced and saved on the user's computer and computers they trust."* Option 2 pushes ADMX /
  plist / Linux JSON policies keyed on the extension ID, with **`Allowed domains`** (permits offline,
  *"but offline editing is turned off by default"*) and **`Auto enabled domains`** (turns it on for
  everyone under the policy). *"Offline access is blocked on any computer that doesn't have the
  policy"*, and switching to policy mode without deploying it means **"users who previously had
  offline access to files will lose access after 24 hours."* Not available for ChromeOS or mobile.
- **Mobile is a different, simpler product.** Android and iOS Docs/Sheets/Slides apps have a single
  settings toggle, **"Make recent files available offline"**, plus per-file *"Make available
  offline"*, plus a dedicated **Offline** list in the navigation drawer (T1). Notably the mobile
  apps expose a *list of what you have offline* that the web does not; the web's equivalent is the
  transient Offline preview mode. The mobile help tabs are roughly a tenth the length of the desktop
  one: no extension, no browser requirement, no document-status indicator, no file-size guidance, no
  one-account-per-profile rule. Admin offline policy does not reach mobile at all — *"these settings
  only apply to Docs, Sheets, and Slides in a Chrome browser on a desktop computer; they have no
  impact on automatic syncing to Android and iOS devices"* (T1, 2017-04-24).
- **One real iOS/Android divergence**, in the *Drive* app (T1): Android says *"tap Make available
  offline"*; iOS says *"To save a **preview** of the file offline, turn on Available offline"* and
  adds *"Tip: To **edit** a Google Docs, Sheets or Slides file offline, use the Google Docs, Sheets,
  or Slides app."* On iOS, Drive gives you a frozen preview and only the editor app gives you an
  editable copy.
- **Whether the mobile apps recalculate offline is not established.** The performance page that
  states offline calculation says "within your **browser**" — it describes the web client only. The
  one circumstantial thread is that Google Sheets for iOS uses J2ObjC to share Java non-UI logic
  (Google Open Source Blog, 2016-01-21, T1), but no source says the calculation engine is among the
  shared code.

**Storage substrate.** I could not establish from any credible source what the offline client
actually writes to disk — IndexedDB database names, Cache Storage keys, whether a service worker is
registered on `docs.google.com`, or whether the extension is Manifest V3. Google does not document
it and I found no trustworthy reverse-engineering. See gaps.

---

## 2. Does it recalculate formulas offline?

**Yes, fully, and this is the single most load-bearing fact in the whole document.**

Google states it directly. *Learn how to improve Sheets performance*, section "How Google Sheets
performs calculations" (T1):

> You can use Google Sheets **without an internet connection**. Your changes are saved within your
> browser and then sent to Google, which means that even when you are offline, you can continue
> using Google Sheets. As you make edits, Google Sheets **performs calculations in the
> background**… **Each time a cell is edited, Sheets evaluates the formula in that cell plus all
> dependent cells.** For example, if B1 has `=A1+1` and A1 changes to `=2+2`, Sheets evaluates A1
> and B1.

Offline operation and dependent-cell evaluation are asserted in the same section of the same page.
That is as explicit as Google gets, and it settles the question.

The mechanism is described in Google's own engineering case study, *"Why Google Sheets ported its
calculation worker from JavaScript to WasmGC"* by Michael Thomas and Thomas Steiner, published on
web.dev, last updated **2024-06-26** (T2, Google-authored):

> The Google Sheets calculation engine was originally written in Java and launched in 2006. In the
> early days of the product, all calculation happened on the server. **However, from 2013, the
> engine has run in the browser using JavaScript.** This was originally accomplished through Google
> Web Toolkit (GWT), and later through Java to Closure JavaScript transpiler (J2CL). The JavaScript
> calculation engine runs in a Web Worker and communicates with the main thread using a
> MessageChannel.

And the companion product announcement, Workspace blog, **2024-06-26** (T1):

> we've doubled the speed of calculation in Sheets on Google Chrome and Microsoft Edge browsers,
> improving the experiences of running formulas, creating pivot tables, using conditional
> formatting, and more… This improved calculation speed is made possible by WasmGC.

So the arc is: **server (2006–2013) → client JavaScript via GWT/J2CL (2013–2024) → client WasmGC
(2024–)**. The offline client contains a complete, production-grade spreadsheet evaluator running in
a Web Worker. Editing offline is not deferred computation; dependent cells recompute locally.

**The date alignment is not a coincidence and is the point.** Calculation moved into the browser in
2013, and *the same release* shipped offline editing — blog.google, **2013-12-11**, by Zach Lloyd,
Software Engineer, titled *"New Google Sheets: faster, more powerful, and works offline"*:
*"Scrolling, loading and calculation are all snappier… No Internet connection? Work offline with
Chrome… you can now make edits to Sheets offline"* (T1). General rollout to all users followed in
March 2014 (TechCrunch, 2014-03-20, T3) — which is the "2014" date most people remember. Google
could not offer offline spreadsheets until it had a client-side calculation engine, and the moment
it had one, it shipped both, in one announcement.

### Google runs two calculation engines, and the offline one is deliberately weaker

This is the sharpest architectural finding in the document, and it comes from a Google patent —
**US11853692B1, "Performing server-side and client-side operations on spreadsheets"**, assignee
Google LLC, inventors including **Zachary Erik Lloyd** (author of the 2013 announcement), priority
date **2013-12-20**, granted 2023-12-26 (T2). It describes racing a client engine against a server
engine, and states the asymmetry explicitly:

> Server calculation engine 306 has at least as much functionality as client calculation engine 312
> and may have additional functionality beyond client calculation engine 312. For example, **server
> calculation engine 306 may have the ability to draw data from external databases for certain
> calculations but client calculation engine 312 may not have this ability.**

(Text verified verbatim against the granted patent, 2026-09-02.)

The patent's core claim is a **race**: send every input to both engines and *"display the result of
the quicker calculation."* Two further passages matter more than the race does.

First, the patent states the offline case in as many words:

> **If there is no connection between client computer 506 and server 502, the document editing
> application is limited to client-side calculations only.**

Second — and this is the striking one — it anticipates the two engines **disagreeing**, and
specifies a tie-break:

> Once client computer 506 receives the slower of the two calculations, it may either ignore the
> slower result or **use the slower result to verify that the quicker result is correct**… Client
> computer 506 or server 502 **may be designated as the master calculator such that when the two
> results do not match, the result of the master calculator is deemed correct.**

So Google did not move calculation to the client. Google **duplicated** calculation onto the client
as a deliberate proper subset, kept the server engine as the authority for anything touching the
outside world, and designed for the case where the two produce different answers for the same input.
The web.dev case study's corpus-replay validation harness is the industrial-scale version of the
same worry. **That is the honest shape of "Sheets recalculates offline": two evaluators, forever,
with a capability gap that maps onto what breaks offline and a known risk of divergence that offline
removes the arbiter for.**

**Caveat, stated plainly:** a patent describes a claimed invention, not necessarily shipped code.
Whether Sheets actually races client against server today is *not* established — the web.dev case
study describes only a client-side worker and does not mention racing. What the patent establishes
reliably is Google's own framing of the problem and the asymmetry they designed around, from the
engineer who announced offline Sheets, dated to the same month it shipped.

**What that cost them, in their own words.** The same case study describes maintaining two engines
and continuously proving them equivalent:

> To ensure that the JavaScript calculation engine produced precisely the same results as the Java
> version, the Sheets team developed an internal validation mechanism. This mechanism can process a
> large corpus of sheets and validate that the results are identical between multiple versions of
> the calculation engine.

They also report the JS engine ran **more than three times slower than the Java server version**,
that the first WasmGC build was **2× slower than the JavaScript one**, and that recovering from
there took roughly two years of optimisation work (prototype end-2021, data early-2022, ship 2024)
with the Chrome V8 and Binaryen teams. Google needed a differential-testing harness over a corpus of
real spreadsheets, a bespoke Java→Wasm compiler, and profiling and heap-dump tooling that did not
previously exist. That is the price of client-side recalculation for a spreadsheet, paid by a team
with a browser vendor in-house.

### Functions that need the network

Google's function help pages were grepped in full for "offline", "internet connection" and "not
available". **Across all of Google's function documentation, "requires an internet connection"
appears exactly once** — on the IMPORTRANGE page, where it also covers two other functions by name
(T1):

> IMPORTRANGE is an **external data function, just like IMPORTXML and GOOGLEFINANCE**. That means it
> **requires an internet connection to work**. Sheets must download the entire range to your
> computer and will be affected by slow network, and is capped at 10MB of received data per request.

| Function | Network dependence | What it renders offline |
|---|---|---|
| IMPORTRANGE, IMPORTXML, GOOGLEFINANCE | **Documented, T1** (quote above) | **Not documented** |
| IMPORTDATA, IMPORTHTML, IMPORTFEED | **Documented, T1** — grouped with the above on the Import-functions usage-limits page, which describes server-side throttling from "too much traffic" | **Not documented** |
| IMAGE | **Not documented.** Self-evident from taking a URL, but its help page says nothing about connectivity | **Not documented** |
| GOOGLETRANSLATE / DETECTLANGUAGE | **Not established even as network-dependent.** Absent from the Web category and from the import-limits page; help pages silent | **Not documented** |
| NOW / TODAY / RAND / RANDBETWEEN | Purely local | **Strongly implied, never stated** — see below |
| Apps Script custom functions | **Documented, T1** — a server round-trip per call | **Not documented**, but see below |
| Add-ons / Extensions | Circumstantial only (Apps Script runs on Google's servers) | **Not documented** |
| `=AI()` (Gemini in Sheets) | **Documented, T1** — server-side | Sidesteps the question — see below |

**Volatiles.** The same performance page says *"Some functions, like TODAY, NOW, and RAND, should be
used sparingly because they're volatile — they are constantly changing, and must be evaluated after
every edit"* (T1). Combined with that page's own offline statement, offline volatile recalculation
is a strong inference. **No source states it.**

**Apps Script custom functions** have documented failure renderings, just not for the offline case
(T1, Apps Script docs): each call *"makes a separate call to the Apps Script server"*; the cell
*"momentarily displays `Loading...`, then returns the result"*; a call that does not return within
30 seconds yields **`#ERROR!`**. And separately, passing a volatile function as an argument makes the
cell display **`Loading...` indefinitely** — so a permanent `Loading...` is a documented real state.
Which of the two you get offline is not documented.

**`=AI()` exists and dodges the problem.** Launched 2025-06-25, Search-grounded from 2025-10-14
(T1). It does **not** auto-recalculate: the user clicks *"Generate and Insert"* / *"Refresh and
Insert"*, and the result is an inserted literal value, not a live formula output. An AI function that
never recalculates has no offline recalculation behaviour to specify.

**The best available proxy for "network path removed" is not an offline page.** Google's
client-side-encryption docs describe what happens to a Sheet whose contents the server cannot read
(T1): *"Features made **static**: … External data functions like GOOGLEFINANCE, Connected Sheets"*
and *"Features removed from the document: … Apps Script and Add-ons."* Google's own word for the
degraded state is **"static"** — a frozen last value, not an error. **This is an encryption page,
not an offline page**, and is offered as suggestive only.

**Net: that these functions cannot fetch offline is certain. What the cell shows — stale value,
`#N/A`, `Loading...`, or an error — Google does not document for a single one of them.** This is the
largest single gap in the public record, and it is closable only by direct observation.

---

## 3. What is degraded or refused offline

**The headline finding is a negative one: Google publishes no list.** After fourteen years of
shipping offline editing, there is no Google page enumerating what does not work offline, on any
platform. The help page's entire treatment of the subject is one line: *"If your document is not
ready to edit offline, an explanation will appear"* (T1). Both offline pages carry no feature
caveats at all. The version-history page and the comments page were each grepped in full: **zero
occurrences of the word "offline" on either.**

What *is* documented:

- **Creating new files offline is supported.** All three platform tabs open with *"If you aren't
  connected to the internet, you can still **create**, view, and edit files on: Google Docs, Google
  Sheets, Google Slides"* (T1). Note the Drive page is narrower — *"view and edit"*, no "create".
  Google does not say what identity a file created offline has before it reaches the server, nor
  what happens if it never does.
- **Comments work offline.** Stated twice: 2012-06-30 (*"edit documents and leave comments
  offline"*) and 2019-04-24 (*"it's now possible to create, edit, and comment on Docs, Sheets, or
  Slides files"*), both T1. This contradicts a lot of secondary writing that lists comments as
  offline-unavailable.
- **Offline edits reach version history.** *"You can find edits in the file's version history"*
  (T1). What timestamps they carry is not stated.
- **Pivot tables, charts, conditional formatting are calc-engine features**, and the 2024 WasmGC
  post groups *"running formulas, creating pivot tables, using conditional formatting"* as things
  the client-side engine does (T1). Strong inference that they work offline; Google never says so
  in an offline context.
- **File size is a limit and the number is unpublished.** Google's documented remedy is telling:
  *"If the error continues, your file is too large… Make your file smaller. Tip: Copy smaller
  sections of the original document into new documents"* (T1). No MB, cell or row threshold appears
  anywhere. The general ceilings (10M cells, 18,278 columns) are not offline-specific.
- **Gemini.** The one Google sentence connecting an AI feature to offline is about state loss, not
  availability: *"You lose your conversation history when: You reload your browser. You close and
  reopen the spreadsheet. **Your computer goes offline.**"* (T1). Google never states that Gemini is
  unavailable offline. A line asserting exactly that circulates widely in search summaries; it
  appears on none of the three Gemini-in-Sheets help pages checked, and should not be quoted.
- **Explore is moot.** The feature was removed around January 2024 (T3); its help URL now serves the
  Gemini page.

Not established either way, because Google is silent: sharing and permission changes, version
history *access*, filters and filter views, data validation, import/export and download-as,
Connected Sheets/BigQuery (whose help page contains no occurrence of "offline"). **The honest
summary is that Google's documentation of offline degradation is essentially absent, and treating
any secondary list of "what doesn't work offline" as authoritative is unwarranted** — most such
lists are reasoning from first principles and presenting it as fact.

---

## 4. Conflict and merge on reconnect

**In its end-user documentation, Google devotes three sentences to this, total.** From Drive Help,
*Use Google Drive files offline*, under the heading "Edit offline files" (T1, fetched 2026-09-02):

> If you edit a file offline:
> - Changes are implemented when you're back online.
> - **New changes overwrite previous changes.**
> - You can find edits in the file's version history.

That middle line is the closest thing to a documented conflict policy Google has, and it is
genuinely vague. It does not define "new" (by wall clock? by arrival order?), does not define the
unit ("changes" — a keystroke? a cell? a document?), and does not say what happens when two offline
clients reconnect. It reads like a warning that offline edits are not specially privileged, not like
a specification.

**Verified absence, not just my failure to find it.** Word-frequency counts across Google's four
canonical pages (the offline page, the Drive offline page, the version-history page, and the
save-status page): **"conflict" appears 0 times**; "merge" appears twice, both about version-history
*compaction*; "overwrit" appears once — the sentence above; "simultaneous" 0 times.

**There is no conflict UI.** No source documents a conflict dialog, a three-way merge view, a
"conflicted copy" file, or any user-facing reconciliation step. Reconnect is silent. This is exactly
the gap that AI-generated articles fill with invented detail.

**But there *is* a documented recovery flow, and it is Sheets-only.** Google's page
[*"Can't save your changes. Please copy your recent edits then revert your changes."*](https://support.google.com/docs/answer/12111392)
is titled with the error string and prescribes (T1): *"Copy all your unsaved changes. Reload the
sheet. Paste in your previous changes."* Note the discrepancy: **Google documents "Can't _save_ your
changes"; every 2026 user report quotes "Can't _sync_ your changes."** Two variants exist and Google
documents one.

### The mechanism, from Google in 2010

Google's own Docs engineering series, **John Day-Richter, September 2010** (T1, Google Drive blog).
Part 3, *"Making collaboration fast"* (2010-09-23), is the load-bearing sentence:

> The first thing the server did, was to **transform John's sent change against all the changes that
> have been committed since the last time John synced with the server**.

Part 2, *"Conflict resolution"* (2010-09-22):

> We save your document as a revision log consisting of a list of these changes… To display a
> document, we replay the revision log from the beginning.
>
> If OT is implemented correctly, it guarantees that once all editors have received all changes,
> everyone will be looking at the same version of the document.

**The word "offline" appears zero times across all three posts.** Offline reconnection is this same
algorithm with a large revision gap; Google demonstrates it only across a two-revision gap and never
extends the example. Google patent **US10678999B2** (Google LLC, Micah Lemonik, priority 2010-04-12,
granted 2020-06-09) recites the same rebase — transform the client's batch against all mutations
since its last sync — with **no bound on how stale the client's revision identifier may be, and no
fallback claim for when it is too old.** "offline" appears 0 times there too.

### Google documents both conflict semantics — but only for developers

The closest thing to a specification Google publishes is the **Docs API `WriteControl`** field
([API best practices](https://developers.google.com/workspace/docs/api/how-tos/best-practices), T1):

> The **requiredRevisionId** field is set to the revisionId of the document the write request is
> applied to. If the document was modified since the API read request, **the write request isn't
> processed and it returns an error.**
>
> The **targetRevisionId** field… If the document was modified since the API read request, **the
> write request changes are applied against the collaborator changes. The result… incorporates both
> the write request changes and the collaborator changes into a new revision… The Docs server is
> responsible for merging the content.**

So Google implements *both* reject-on-conflict and transform-and-merge, and exposes the choice — to
API developers. **The offline editor never surfaces that choice to a user.** It is always
transform-and-merge, silently.

**The merge is Operational Transformation, and Google once documented what that costs.** The best
source is Google's own **Drive Realtime API** documentation — the public exposure of Google's
collaborative-editing infrastructure, deprecated and shut down around 2018, read here via the
Internet Archive's 2018 capture (T1, archived):

> Rather than providing ACID guarantees using locking or transactions, conflicts and changes from
> different collaborators are automatically resolved using a process called **operational
> transformation**… The data model is guaranteed to be **eventually consistent**: all users can
> write to the document at the same time, and any conflicts are automatically resolved such that the
> model eventually has the exact same state on all clients and the server. **However, it is possible
> to design models that, as a result of collaboration, can end up in a state that your application
> can no longer understand.**

That last sentence is Google conceding the central limitation of OT in its own developer docs:
**convergence is guaranteed; sense is not.** The page then gives a worked example that is directly
transferable to spreadsheet rows. Alice and Bob each set `name` and `phone` on the same record
inside a compound operation; Bob also sets `address`. The documented outcome:

```
{ 'name': 'Alice',            // Alice's name
  'phone': '555-5309',        // Alice's number
  'address': 'Anytown, USA' } // Bob's address!
```

Google's own exclamation mark. Two authors each wrote a coherent record; the merge produced a third
record that neither wrote. Google is explicit that grouping does not save you — *"they are not
atomic groups, and they are not transactions"* — and that the only way to prevent it is to give up
merging for those fields entirely, by encoding them as a single non-collaborative object, because
*"these objects are always written as an atomic unit, so changes between two different collaborators
are never merged and **one collaborator's version wins**."*

The example Google chooses to motivate this is telling: an X and a Y coordinate, where *"it never
makes sense to merge the X coordinate from one user's clicks with the Y coordinate from another."*

**A spreadsheet row is that case.** A row is a set of cells that mean something together, and OT
over a spreadsheet's *structural* operations — insert row, delete column — against *value* edits is
the hard version of it. That is exactly what `SPIKE-FORK-MERGE.md` in this repo found to be
non-commuting. Google does not document its transform functions for Sheets, and I found no source
that does; the Realtime API docs describe a general collaborative data model, not Sheets cells, and
should not be read as a specification of Sheets behaviour. But they are Google, on the record, about
the algorithm family it uses, saying that automatic merge of jointly-meaningful fields produces
records nobody authored — and offering "don't merge, let one side win" as the only remedy.

**Google publishes nothing about how offline op batches are ordered against online ones.** Note also
that the Realtime API warns *"every collaborator may observe very different timings and orderings of
edit events"* — ordering is not wall-clock and is not promised to anyone.

The underlying OT framework is documented in the Google Wave OT whitepaper (Wang, Mah, Lassen,
v1.1, July 2010, T2 — Google-authored), which remains the clearest public description of the
concurrency control Google Docs inherited.

### The gap between what Google documents and what Google's forum says

Google's own volunteer Product Experts answer offline-loss threads with a canned paragraph that has
appeared verbatim across dozens of threads from 2019 to 2026 (T3 — a volunteer, not a Google
employee, but a designated one). Verbatim, from a thread dated **2026-08-24**:

> Offline access data is only **TEMPORARILY** stored on the device that the offline access was done
> on… **If you connect with a different device, then the offline access data will be overwritten by
> the last version saved from the device that you reconnect with.** If you restart the device, then
> the offline access data may be purged. If you reset the browser or any extension, then the offline
> access data may be purged.

**None of those three loss conditions appears anywhere in Google's documentation.** The same expert,
2023-04-06: *"Due to this volatility, I do not rely on offline editing… and I would never recommend
it to anyone."*

### Failure modes, dated

| Date | What happened |
|---|---|
| **2026-06-28** | Several days offline. On reconnect: *"The application is out of date. You need to reload this page to continue editing. **All of your changes have been saved.**"* → reload → *"all the text became scrambled up"* → "file has been corrupted" + Revert → **document blank, all work lost.** The reassurance is emitted immediately before the destruction. |
| **2026-06-09** | A week of fieldwork, 24 sheets, Android tablet. On reconnect: white screen plus *"Can't sync your changes. Please copy your recent edits, then revert your changes."* — *"I cannot copy or screenshot or transcribe because it is a white screen."* **The documented recovery flow was physically impossible to execute.** |
| **2026-07-30** | Slides, ~1 month offline, 1,100+ slides. Same error. *"'Make a copy' creates the old version, downloads do not work."* |
| **2026-02-28** | *"when I came back on it online to work on it (on a separate device but still the same account) **all my offline work was gone**"* |
| **2013-09-16** | Two days offline: *"google wiped away my 2 days of hard work **because another author made a minor edit**."* Oldest and most on-point report for the exact online-vs-offline collision; single, uncorroborated. |
| **2013-01-05** | Issue Tracker: *"Offline editing destroyed on a sync"*, P2. *"It now said there is a **version conflict**, and asked what to do… they were both the same. All offline edits were lost."* Closed **Won't fix (Obsolete)** in a bulk sweep, 2013-05-22. |
| **2015-07-13** | Issue Tracker, filed by a **Chromium engineer** with a clean repro, P1: *"See the reassuring text 'All changes saved offline'… Actual: the new doc is not visible anywhere, and any edits… seemingly lost."* Still marked *Duplicate*. |

**This is current, not historical.** Google's official forum surfaced roughly **23 distinct
offline-data-loss threads in 2026 alone** (Jan–Sep), including one dated 2026-09-01. There is no
live public bug component — the only two Issue Tracker entries are from 2013 and 2015, bulk-closed
or still marked Duplicate. Every thread dead-ends with an unpaid volunteer saying that if it is not
in version history, it is gone. **The extension's 2.4/5 rating across 8.9K ratings is the most
defensible aggregate signal available.**

**Google's documented remedy when editing breaks** appears in *Troubleshoot errors while you edit*
(T1): make a copy, or download and re-upload — with the explicit warning **"You'll lose some
features, like version history, in your copied file."**

So Google *does* own a fork primitive. It reserves it for disaster recovery, and it tells you what
it costs.

### The hard bound nobody publishes: server op-log retention

OT rebase requires the server to still hold every operation since the client's last sync. Google's
2010 series confirms the storage model is a revision log replayed from the beginning; the Drive API
concedes that for *"frequently edited Google Docs, Google Sheets, and Google Slides… Older revisions
might be omitted"* (T1). Joseph Gentle, an ex-Google-Wave engineer, states the consequence plainly
(2020-09-26, T2): *"The only storage overhead is the operation log, and you can trim that down if you
want to. **(Though you can't merge super old edits if you do)**"*, and *"if google's servers say no,
I lose my changes."*

**Server revision-log retention is the real limit on how stale an offline branch may be. Google has
never published that retention policy, nor what happens when a branch outlives it.** The 2026 report
of a one-month-offline Slides deck failing to sync is consistent with hitting exactly this wall, but
that is inference, not a sourced diagnosis.

---

## 5. Storage and eviction

**Google publishes no cap, no eviction policy, and no eviction notice.** Every statement is a
gesture at free space: *"Make sure you have enough available space on your device"*, *"If you have
enough storage, some of your most recent files will be automatically saved offline"* (both T1). The
Chrome Web Store's "hundreds of your most relevant documents" is the only quantity Google states
anywhere, and it is marketing copy, not a specification.

What follows from that, and what does not:

- **The automatic set must have an eviction policy**, because it is bounded ("a certain number",
  "hundreds") and ordered by recency. Google never describes it, never surfaces it, and gives the
  user no notification when a file leaves the set. The only way to find out is Offline preview,
  which you have to think to check while you still have a network.
- **Pinned files are described as permanent** — *"so that it always remains available offline"*
  (T1, 2019). Whether a pin survives browser storage eviction under disk pressure is not documented.
- **Per-file size limits exist and are unpublished** (§3).
- **ChromeOS documents an automatic shut-off, and it is the only eviction-shaped rule Google
  states anywhere**: *"If your available storage on the device runs low, the feature turns off
  automatically"* (Chromebook Help, T1). Note what that says — under storage pressure ChromeOS does
  not evict some files, it **disables offline access wholesale**. Whether the web client on Windows
  or macOS behaves the same way is not documented.
- **Browser quota turns out not to apply at all**, which is the most interesting thing in this
  section — see below.
- **The one documented time bound is administrative, not storage:** switching a Workspace domain to
  device-policy mode without deploying the policy means *"users who previously had offline access to
  files will lose access after 24 hours"* (T1, 2026-08-26). So there is a ≤24h heartbeat by which a
  client re-checks entitlement — which also implies an offline client cannot stay offline
  indefinitely and retain access under that configuration.
- **A widely-repeated claim that offline access "expires after about 30 days"** appears in secondary
  sources. I found **no** Google documentation supporting it and could not verify it. Treat as
  unconfirmed.

### `docs.google.com` is exempt from Chrome's storage quota entirely

**Undocumented by Google; established by reading the shipping extension and the Chromium source
(T2).** The live CRX for **Google Docs Offline v1.109.1** (2026-08-20) contains:

```json
"content_capabilities": {
  "matches": ["https://docs.google.com/*", "https://drive.google.com/*", …],
  "permissions": ["clipboardRead", "clipboardWrite", "unlimitedStorage"]
}
```

`content_capabilities` grants those permissions **to the matched web origins**, not to the extension
itself. The Chromium plumbing confirms what that buys, at tip-of-main:

- `chrome/browser/extensions/extension_special_storage_policy.cc` — an extension whose
  `content_capabilities` include `unlimitedStorage` causes its matched origins to be added to
  `content_capabilities_unlimited_extensions_`, and `IsStorageUnlimited()` returns true for them.
- `storage/browser/quota/quota_database.cc`, in the LRU eviction selector:
  ```cpp
  if (is_default && special_storage_policy &&
      (special_storage_policy->IsStoragePersistent(read_gurl) ||
       special_storage_policy->IsStorageUnlimited(read_gurl))) {
    continue;   // skipped — never evicted
  }
  ```

Consequences, none of which Google documents:

1. `docs.google.com` has **no quota ceiling** — not the usual fraction-of-disk, and not the figure
   `navigator.storage.estimate()` reports.
2. Its default bucket is **never selected for LRU eviction, at any storage pressure**, including
   disk-full.
3. Docs never needs `navigator.storage.persist()`; this capability is strictly stronger.
4. **The exemption is contingent on the extension staying installed.** Uninstall or disable Google
   Docs Offline and `docs.google.com` reverts to an ordinary best-effort origin — now carrying a
   large footprint, making it a *prime* eviction candidate. Nobody documents this cliff.

**So the realistic destruction path is not browser eviction — it is the user being told to clear
site data.** Google's own troubleshooting page prescribes exactly that, twice, as the remedy for
*"Checking offline sync status. Please wait."* and *"Offline setup failed"*:

> **Clear your site data:** Open `chrome://settings/cookies/detail?site=docs.google.com` … Click
> **Remove all**.

**That destroys every offline document, including unsynced local edits, and the page carries no
warning.** (T1 for the instruction; the consequence is inference from how origin data clearing
works, but it is not a subtle inference.)

**Storage backend:** no Tier 1 or Tier 2 source names it. Web SQL was removed in Chromium 119 and
AppCache around Chrome 95, and this extension ships in 2026, so by elimination it is IndexedDB
and/or Cache Storage. **Treat "IndexedDB" as inference, not a sourced fact.**

---

## 6. The identity question

**You are editing the file, not a copy of it.** Nothing in Google's model mints a second identity.
The URL is the same, the document ID is the same, and collaborators' view is of the same object.

**Version history is *supposed* to be continuous across the offline period, and reportedly is not.**
Google says only *"You can find edits in the file's version history"* (T1) and never states what
timestamps offline edits carry. The version-history help page contains **zero** occurrences of the
word "offline". Two T3 reports contradict continuity: one (2026-01-24) that version-history
timestamps for offline edits were **shifted forward by days to a week** — i.e. the audit trail
records reconnect time, not edit time — and several that the offline edits are simply *absent*
(2026-04-28: *"it says I didn't even edit it at the time I wrote it"*). Compounding this, Google
documents that history is lossy by design: *"The revisions for your file may occasionally be
merged"*, avoidable only by creating named versions; and the Drive API warns that for *"frequently
edited Google Docs, Google Sheets, and Google Slides… Older revisions might be omitted"* (both T1).

**The honest characterisation** is that the user is editing a *device-local, volatile, unnamed
branch* that Google's UI presents as the file itself. That mismatch is exactly what Google's own
Product Experts warn about, and it is what makes the loss reports read as betrayal rather than
inconvenience.

**The UI says almost nothing about which.** Google's one gesture at it is the 2020 document-status
indicator, whose stated purpose is *"More descriptive text to indicate whether a document is saved
to the cloud (when online) or to the device (offline)"* (T1, 2020-06-08). That distinguishes *where
the bytes currently are*, not *what you are editing*. The announcement is explicit that it changed
nothing underneath: *"This is an update to the interface only — there are no changes in the
underlying functionality."*

**The one place Google's own language says "copy" is the patent, and it says the opposite of fork.**
US11853692B1 describes the client state as *"Server 302 loads a **copy** of spreadsheet 304 onto
client computer 308, shown as **local copy** 314. **Edits made to local copy 314 are synchronized
with** spreadsheet 304 stored on server 302… **Multiple users may access spreadsheet 304 and server
302 is responsible for reconciling edits made by each user.**"* (T2). That is a replica with a
server-side reconciler — a cache with an outbox, described in patent language. It is precisely not a
fork: it has no independent identity and no user-visible reconciliation.

**Is there any sense in which Google's model is a fork? No.** It is a cache plus an outbox over one
canonical document. The evidence is uniform:

- Google never creates a second file from offline editing, in any documented flow.
- The user's only affordances are *materialisation* controls — "Make available offline", "pin" —
  never *identity* controls.
- Google's fork primitive is `Make a copy`, it lives in a different menu, it is documented as an
  error-recovery step, and it carries an explicit warning that you lose version history. Google
  knows what forking costs and declines to pay it for offline.

The nearest thing to a fork-shaped surface anywhere in the system is the mobile apps' dedicated
**Offline** list in the nav drawer — a place where your offline set is a *thing you can look at*.
Even there, opening an item opens the document, not a copy.

---

## What this implies for a fork-based design

**1. The most important finding is about the calculation engine, not about offline scoping.**
Google's ability to offer offline spreadsheet editing at all is downstream of one decision: in 2013
they put a formula evaluator in the browser, and they have been paying to keep it there ever since —
GWT, then J2CL, then a bespoke Java→WasmGC toolchain built jointly with the V8 and Binaryen teams,
plus a differential-testing harness that replays a corpus of real spreadsheets to prove two engines
agree. That is not a feature Google added to support offline; **offline is a side effect of having
done it**, and the two shipped in the same December 2013 release because one enabled the other.

And it is worse than "they moved calculation to the client", which is how the case study reads at a
glance. The patent makes clear they **duplicated** it: *"Server calculation engine 306 has at least
as much functionality as client calculation engine 312… server calculation engine 306 may have the
ability to draw data from external databases… but client calculation engine 312 may not have this
ability."* Google has run **two evaluators with a deliberate capability gap between them since
2013**, and that gap is not incidental — it maps almost exactly onto the set of things that break
when you go offline. Twelve years later they still cannot tell you what `GOOGLEFINANCE` renders when
the network is gone, because that behaviour lives in the seam between two engines nobody documents.

For a server-rendered hypermedia design with no client-side application code, this cuts cleanly and
in one direction: **Google's offline model is unavailable to you, entirely, and not because of
scoping choices — because of the evaluator.** A cache-plus-outbox over one canonical document
requires the client to render a *correct* view of the document as edited, and for a spreadsheet
"correct" means recalculated. No evaluator, no live cell values, no cache-plus-outbox. This repo's
own `SPIKE-WASM-STANDIN.md` measured what shipping the server into a service worker costs — 4.83 MB
gzip all-in — and Google's history is the independent corroboration that the price is not a one-off
download cost but a permanent second implementation to keep honest.

That is an argument *for* a fork, but not the argument people usually make. It is not "forks are
better UX". It is: **a fork with a real server behind it is the only shape that gets correct
recalculation from exactly one evaluator.** If the fork is a genuine copy that a server owns — even
a local one — `recalc.go` is the evaluator, and there is one of it. Google's alternative was to
build a second one, accept that it is a strict subset of the first, and spend eleven years proving
the overlap still agrees.

**2. Google's model is not a fork, and the thing it buys by not forking is the thing you lose.**
What Google gets from cache-plus-outbox is that the offline case is *invisible*. There is no second
identity for the user to track, no reconciliation step to perform, no moment where they must decide
what happened. The document is the document. Users need no mental model at all — which is why the
automatic layer can be automatic in the first place: **you can silently cache hundreds of documents
because caching a document commits the user to nothing.** You cannot silently fork hundreds of
documents. A fork is an act, and acts need consent, so a fork-based design cannot have an automatic
floor, and `research/offline-scoping-patterns.md` already establishes that explicit-only-with-no-floor
is the configuration users complain about.

**3. What it costs Google to not fork is documented — in one vague user-facing sentence, and in one
very frank developer-facing page.** The user-facing version is *"New changes overwrite previous
changes."* The developer-facing version is Google's Realtime API docs conceding that OT gives
convergence but that a model can *"end up in a state that your application can no longer
understand"*, and demonstrating it with a merged record that neither author wrote.

For a text document that trade is survivable: OT for text is mature, and the damage from a bad
transform is a garbled paragraph a human can see. For a spreadsheet with structural operations,
blind convergence is far more dangerous — a wrongly-transformed row insert silently misaligns a
column and the result is *plausible*, not visibly broken. Google's answer is to publish nothing about
it for Sheets, provide no conflict UI, keep version history as the escape hatch, and let the 2.4/5
extension rating absorb the complaints. **That is a defensible trade for prose and a poor one for a
spreadsheet.**

**And the strongest evidence for the fork is Google's own recommended workaround.** When fields must
change together, Google's advice in its own docs is to stop merging them: make them a single atomic
non-collaborative object, at which point *"changes between two different collaborators are never
merged and one collaborator's version wins."* Google reaches for coarse-grained, whole-object,
one-side-wins semantics the moment automatic merge would produce nonsense. **A fork is that same
move at document granularity, with the crucial improvement that the losing side is not thrown away
but kept and reconciled deliberately.** That is a defensible position, and it should be argued as
*conflict policy* — which is where Google's own evidence supports it — not as offline scoping, where
it does not.

**4. Where Google's evidence argues against the design as stated.** Three things:

- *"Take a copy offline" reads like "Make available offline" and behaves like `git clone`.* Google
  has trained fourteen years of expectation that this control materialises, and its own
  identity-changing control (`Make a copy`) is in a different menu with a warning attached. If a
  fork is minted, the verb must not be borrowed from the materialisation family.
- *Google's pin is additive and reversible; a fork is neither.* Un-pinning costs nothing.
  Un-forking requires a merge.
- *Google offers a floor and the fork design cannot.* This is the substantive gap, and the fix is
  not to abandon the fork but to accept that anticipated-offline and unanticipated-offline are two
  different products, as the companion document argues.

**5. A fork has one structural advantage Google's model cannot have, and it is not the one usually
claimed.** OT rebase requires the *server* to retain every operation since the client's last sync.
Google's storage model is a revision log; the Drive API concedes older revisions get omitted on
busy documents; Wave's own engineer says plainly that trimming the log means *"you can't merge super
old edits."* **So in Google's model the maximum survivable offline duration is a property of
server-side retention that nobody publishes and the user cannot see.** A one-month-offline deck that
fails to sync is consistent with hitting exactly that wall.

A fork does not have this failure mode. A fork carries its own complete state; it does not need the
origin to remember anything about it, and it cannot expire. That is a real and underrated argument,
and it is *specifically* an argument about long, anticipated offline periods — which is the case the
fork design is aimed at. It says nothing about the five-minute network drop.

**6. Google implements both conflict semantics and hides the choice.** The Docs API exposes
`requiredRevisionId` (reject if the document moved) and `targetRevisionId` (transform and merge).
Both exist in Google's server today. The offline editor always picks the second, silently, and never
tells the user which it used or that there was a choice. A fork-based design is essentially choosing
`requiredRevisionId` semantics at document granularity and then giving the user somewhere to stand
while they resolve it — which is the one thing Google's API leaves entirely to the caller and
Google's own product never offers.

**7. One place Google's constraints do not bind you.** Google is Chrome-and-Edge-only in 2026 for a
reason that no longer holds technically — WasmGC has been Baseline since December 2024 — but does
hold logistically, because delivery is a Chrome Web Store extension. A server-rendered design has no
equivalent trap: there is nothing to install, and browser support is not a gate. That is a real
advantage and it is worth being explicit that it comes free.

---

## What I could not establish

For a closed-source product this list is long, and several entries are things a reasonable person
would expect to be documented and are not.

**Google does not document, at all:**

- **Any list of what is degraded or refused offline.** Fourteen years, no such page. Every secondary
  list I found is inference presented as fact.
- **What network-dependent functions display offline.** `IMPORTRANGE`, `GOOGLEFINANCE`, `IMAGE`,
  `GOOGLETRANSLATE`, Apps Script custom functions, add-ons, Connected Sheets — stale value vs `#N/A`
  vs `Loading...` vs error is undocumented for **every single one**, and I found no credible test.
  This is the largest gap in the record. Two weak signals point opposite ways: Google's
  client-side-encryption page calls the degraded state **"static"** (frozen value), while Apps
  Script documents both `Loading...` (indefinitely, in one case) and `#ERROR!` as real states.
- **Whether `GOOGLETRANSLATE` / `DETECTLANGUAGE` are even network-dependent.** Not established in
  either direction; absent from every grouping Google uses for external-data functions.
- **`IMAGE`.** Network need is self-evident from the fact it takes a URL; Google's documentation for
  it says nothing about connectivity at all.
- **Whether volatile functions (`NOW`, `TODAY`, `RAND`) recompute offline.** Almost certainly yes
  given they live in the calc engine, but no source says so.
- **Whether the mobile apps recalculate offline**, or contain a calculation engine at all. The one
  Google statement about client-side calculation says "within your browser".
- **Any conflict semantics for Sheets beyond "New changes overwrite previous changes."** No
  definition of "new", no unit, no ordering rule, no statement of what happens when two clients are
  offline simultaneously. The Realtime API pages quoted in §4 describe a general collaborative data
  model, not Sheets cells; they establish the algorithm family and its documented failure shape,
  **not** Sheets' actual transform functions, which Google has never published.
- **How offline edits are timestamped in version history** — at edit time or at reconnect time.
  Google says nothing. The only dated evidence (T3, 2026-01-24) reports timestamps shifted forward
  to reconnect time, which is the opposite of what secondary sources assert. One report is not
  enough to call it.
- **Any storage cap or eviction notification.** Google states no byte, file-count or per-file limit
  anywhere. (The *eviction* half of this is now answered, but by reading Chromium rather than by
  anything Google published — see §5.)
- **The `unlimitedStorage` grant and the quota exemption it confers on `docs.google.com`**, or the
  cliff that appears when the extension is uninstalled. Undocumented by Google entirely.
- **That clearing site data — which Google's own troubleshooting page prescribes twice — destroys
  every offline document and every unsynced edit.** No warning on the page.
- **Server revision-log retention**, and what happens when an offline branch outlives it.
- **The per-file size limit for offline sync.** Google documents only the symptom and the workaround.
- **What identity a file created offline has before it first reaches the server.**
- **Whether offline behaviour differs for shared files, files you don't own, or shared drives.** I
  found no Google statement either way, and could not read the one community thread on the subject.

**Things I could not resolve from any source:**

- **The offline storage substrate, precisely.** No Tier 1 or Tier 2 source names it. Web SQL
  (removed in Chromium 119) and AppCache (removed ~Chrome 95) are excluded by elimination, leaving
  IndexedDB and/or Cache Storage — **inference, not a sourced fact.** I did not establish database
  names, whether a service worker is registered on `docs.google.com`, or whether the extension is
  Manifest V3. What *is* now established (§5) is the quota policy around that storage, which turned
  out to matter more. Separately: the calc engine runs in a **Web Worker** over a `MessageChannel`
  (T2) — a different mechanism from the persistence layer.
- **When Sheets first got offline editing — Google's own record contradicts itself.** The
  2013-01-23 Slides post says offline editing of *"Docs and Sheets"* was already enabled; the
  2013-12-11 post announces *"you can now make edits to Sheets offline."* Best reading: some offline
  editing existed in old Sheets by early 2013, and the December post concerns the **rewritten**
  Sheets. **I would not state a single date for "Sheets went offline".** The claim that matters here
  — that client-side calculation and offline shipped together in the rewrite — is unaffected.
- **The actual number of auto-cached files.** "A certain number" (2019) and "hundreds" (Chrome Web
  Store, 2026) are the only figures Google has ever given, and "relevant" in the store copy hints at
  something more than pure recency — possibly the same signals Drive uses for its Priority ranking.
  Unconfirmed.
- **The claim that offline access expires after ~30 days.** Widely repeated, unsupported by any
  Google page I could find. The only documented time bound is the 24-hour admin-policy revocation.
- ~~When the Docs Offline extension stopped being bundled with Chrome.~~ **Resolved** — it was never
  bundled in the binary; see §1. What remains unestablished is *why* the help pages reversed in
  late 2018. The Chrome Apps platform sunset and the removal of the Gmail Offline Chrome app
  (2018-09-12) are suggestive of a common cause, but no source links them.
- **Whether the Sheets calc engine reached Firefox or Safari.** Google said in June 2024 they
  expected to, WasmGC went Baseline in December 2024, and Google's docs still say Chrome/Edge in
  September 2026. No announcement either way.
- **Google Docs Editors Help community threads.** These are JavaScript-rendered and returned no text
  to either fetching method. Several thread titles are directly on point (*"Multiple people editing
  the same doc offline — how are changes synced?"*, *"Offline edits failed to sync"*, *"Can Shared
  Drives be made available offline?"*, *"Google Sheets — Download Offline Size Limit?"*). **They
  should be read in a browser**; they are the most likely source of real answers to several items
  above.
- ~~Whether there has genuinely been no recent offline announcement.~~ **Resolved** — a full-text
  search of the Workspace Updates Blogger feed (72 posts matching "offline", 2009–2026) puts the
  last browser-offline post at **2022-06-27**. Four years of silence, established rather than
  assumed.

**Method and coverage limits, for anyone extending this:**

- Google help pages were read by direct HTTP fetch and text extraction, which works well for
  `support.google.com` articles and `knowledge.workspace.google.com`. It does **not** work for the
  Google Docs Editors Help **Community** threads, which are JavaScript-rendered and returned no
  body text to either method. Those threads are the most likely source of answers to several gaps
  above and should be read in a browser.
- `patents.google.com` bot-blocks (HTTP 503); the patent text quoted here was verified via
  freepatentsonline.
- `web.archive.org` rate-limited (HTTP 429) partway through, so some help-page revision dates could
  not be bracketed as tightly as others. In particular I could not date when the *"New changes
  overwrite previous changes"* line was added to the Drive help page.
- Reply and upvote counts are not extractable from Google forum HTML, so the volume of "me too" on
  the loss threads is unquantified beyond explicit quotes. The ~23 threads figure for 2026 is a
  floor, not a count.
- No Google postmortem or status-page entry exists for Docs offline sync loss. The widely-cited
  **November–December 2023 Drive data-loss incident was Drive for Desktop, not Docs offline
  editing** — the two are frequently conflated and should be kept separate.
- **The highest-value next step is not more searching, it is one hour of direct observation:** turn
  on Docs Offline, build a sheet containing `IMPORTRANGE`, `GOOGLEFINANCE`, `IMAGE`,
  `GOOGLETRANSLATE`, `NOW`, `RAND` and an Apps Script custom function, pin it, kill the network, and
  record what each cell renders and what the status indicator says. Then edit a cell offline in one
  browser profile while editing the same cell online in another, reconnect, and read version
  history. Almost every open question above falls to that experiment, and none of it is documented
  anywhere.

---

## Sources

**Tier 1 — Google**

- Work on Google Docs, Sheets, & Slides offline — Computer / Android / iPhone & iPad:
  https://support.google.com/docs/answer/6388102 (fetched 2026-09-02)
- Use Google Drive files offline — Computer: https://support.google.com/drive/answer/2375012 (fetched 2026-09-02)
- Set up offline access to Docs, Sheets & Slides (Workspace admin; last updated 2026-08-26):
  https://knowledge.workspace.google.com/admin/drive/set-up-offline-access-to-docs-sheets-and-slides
  (redirected from https://support.google.com/a/answer/1642623)
- Troubleshoot errors while you edit Google Docs, Sheets, Slides, Vids, or Pics:
  https://support.google.com/docs/answer/7505592
- Use your Chromebook offline (for the Gmail-offline day-window contrast):
  https://support.google.com/chromebook/answer/3214688
- Learn about system requirements & browsers: https://support.google.com/docs/answer/2375082
- Chrome Web Store — Google Docs Offline, v1.109.1, updated 2026-08-20:
  https://chromewebstore.google.com/detail/google-docs-offline/ghbmnnjooekpmoecnnnilnnbdlolhkhi
- Workspace Updates, 2012-06-30 — Offline Document editing:
  https://workspaceupdates.googleblog.com/2012/06/offline-document-editing.html
- Workspace Updates, 2017-04-24 — Improved admin controls over offline access:
  https://workspaceupdates.googleblog.com/2017/04/improved-admin-controls-over-offline.html
- Workspace Updates, 2019-04-24 — Work anywhere with Google Docs, Sheets, and Slides in new offline mode:
  https://workspaceupdates.googleblog.com/2019/04/drive-offiline-mode.html
- Workspace Updates, 2020-06-08 — New document save status and offline indicator:
  https://workspaceupdates.googleblog.com/2020/06/new-save-status-online-offline-google-docs.html
- Workspace Updates, 2021-09-02 — Easily make all file types available offline in Google Drive:
  https://workspaceupdates.googleblog.com/2021/09/easily-make-all-files-types-available.html
- blog.google, Dec 2013 — New Google Sheets: faster, more powerful, and works offline:
  https://blog.google/products/docs/new-google-sheets-faster-more-powerful/
- Workspace blog, 2024-06-26 — Doubling calculation speed and other new innovations in Google Sheets:
  https://workspace.google.com/blog/sheets/new-innovations-in-google-sheets
- Learn how to improve Sheets performance (the offline-calculation statement):
  https://support.google.com/docs/answer/11468464
- IMPORTRANGE (the one "requires an internet connection" in all of Google's function docs):
  https://support.google.com/docs/answer/3093340
- Learn more about Import functions (usage limits / throttling for the IMPORT* family):
  https://support.google.com/docs/answer/12188454
- Custom functions in Google Sheets (Apps Script; `Loading...` and `#ERROR!` states):
  https://developers.google.com/apps-script/guides/sheets/functions
- Client-side encryption — features "made static" vs "removed" (offline proxy, not an offline page):
  https://support.google.com/docs/answer/10519333
- Collaborate with Gemini in Google Sheets ("You lose your conversation history when… your computer
  goes offline"): https://support.google.com/docs/answer/14356410
- Generate data with the AI function in Sheets: https://support.google.com/docs/answer/15877199
  (launched 2025-06-25, Search-grounded 2025-10-14 per Workspace Updates)
- Google Sheets size limits (10M cells / 18,278 columns): https://support.google.com/docs/answer/37603
- Use Google Drive files offline on your Chromebook (auto shut-off under low storage):
  https://support.google.com/chromebook/answer/2809731
- Google Open Source Blog, 2016-01-21 — J2ObjC 1.0 (Sheets for iOS shares Java logic):
  https://opensource.googleblog.com/2016/01/j2objc-10-release_21.html

- Workspace Updates, 2013-01-23 — View and edit Slides offline:
  https://workspaceupdates.googleblog.com/2013/01/view-and-edit-slides-offline.html
- Google Drive blog, 2013-12-11 — New Google Sheets:
  https://drive.googleblog.com/2013/12/newsheets.html
- Workspace Updates, 2015-02-03 — Offline access auto-enabled when signing into Chrome (with the
  2015-02-25 "on hold" update appended):
  https://workspaceupdates.googleblog.com/2015/02/offline-access-to-google-docs-editors.html
- Workspace Updates, 2022-06-27 — Offline syncing for opened Microsoft Office documents (the last
  browser-offline announcement to date)
- Docs API best practices — `WriteControl`, `requiredRevisionId` vs `targetRevisionId`:
  https://developers.google.com/workspace/docs/api/how-tos/best-practices
- Drive API, Manage file revisions ("Older revisions might be omitted"):
  https://developers.google.com/workspace/drive/api/guides/manage-revisions
- "Can't save your changes. Please copy your recent edits then revert your changes." (the Sheets-only
  documented recovery flow): https://support.google.com/docs/answer/12111392
- Find what's changed in a file (version history; zero occurrences of "offline"):
  https://support.google.com/docs/answer/190843
- Drive FAQ for admins — Docs/Sheets/Slides on disk are "essentially just pointers to web documents":
  https://support.google.com/a/answer/2490100
- Google Drive blog, September 2010 — John Day-Richter's three-part series on the new Docs editors,
  including "Conflict resolution" (2010-09-22) and "Making collaboration fast" (2010-09-23):
  https://drive.googleblog.com/2010/09/whats-different-about-new-google-docs.html

**Tier 2 — Google engineering write-ups, patents, and source**

- web.dev case study, last updated 2024-06-26 — Why Google Sheets ported its calculation worker from
  JavaScript to WasmGC (Michael Thomas, Thomas Steiner):
  https://web.dev/case-studies/google-sheets-wasmgc
- US Patent US11853692B1 — "Performing server-side and client-side operations on spreadsheets",
  Google LLC, inventors incl. Zachary Erik Lloyd, priority 2013-12-20, granted 2023-12-26. The
  two-engine asymmetry, the offline fallback, and the master-calculator tie-break:
  https://patents.google.com/patent/US11853692B1/en
  (patents.google.com returned 503 / bot-blocked; text verified verbatim via
  https://www.freepatentsonline.com/11853692.html)
- Google Wave Operational Transformation whitepaper — David Wang, Alex Mah, Soren Lassen, v1.1,
  July 2010:
  https://svn.apache.org/repos/asf/incubator/wave/whitepapers/operational-transform/operational-transform.html
- US Patent US10678999B2 — "Real-time collaboration in a hosted word processor", Google LLC, Micah
  Lemonik, priority 2010-04-12, granted 2020-06-09. Transform-against-history on reconnect, with no
  staleness bound: https://www.freepatentsonline.com/10678999.html
- Chromium source (tip-of-main) — the storage-quota exemption:
  `chrome/browser/extensions/extension_special_storage_policy.cc` and
  `storage/browser/quota/quota_database.cc`
- Google Docs Offline extension CRX v1.109.1 manifest — the `content_capabilities` /
  `unlimitedStorage` grant to `docs.google.com` and `drive.google.com`
- Joseph Gentle (ex-Google Wave engineer, ShareJS author), "I was wrong. CRDTs are the future",
  2020-09-26 — op-log trimming and the merge horizon: https://josephg.com/blog/crdts-are-the-future/

**Tier 1 — archived Google documentation**

- Google Drive Realtime API, "Conflict Resolution and Grouping Changes" (service deprecated ~2018;
  read via the Internet Archive's 2018 capture). Source of the OT statement, the eventual-consistency
  caveat, the merged-record worked example, and the "one collaborator's version wins" remedy:
  https://web.archive.org/web/2018/https://developers.google.com/google-apps/realtime/conflict-resolution

**Tier 3 — dated press**

- The Register, 2010-04-14 — Offline Google Docs disappear on May 3 (quotes Google's blog post):
  https://www.theregister.com/2010/04/14/google_docs_to_lose_offline_support/
- TechCrunch, 2011-08-31 — Google's New HTML5 Chrome Apps For Gmail, Calendar And Docs Give Users
  Offline Access:
  https://techcrunch.com/2011/08/31/googles-new-html5-chrome-apps-for-gmail-calendar-and-docs-give-users-offline-access
- 9to5Google, 2019-04-24 — Google Drive on the web gains new 'offline preview mode':
  https://9to5google.com/2019/04/24/google-drive-new-offline-mode/
- 9to5Google, 2020-05-27 — Google Docs get new 'document status' indicator online:
  https://9to5google.com/2020/05/27/google-docs-document-status/
- Neowin, July 2023 — Microsoft Edge force-installs Google Docs Offline extension without permission:
  https://www.neowin.net/news/microsoft-edge-force-installs-google-docs-offline-extension-without-permission/
