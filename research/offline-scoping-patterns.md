# Offline scoping policies in collaborative apps

Research date: **2026-09-02**. All findings are from web sources fetched on this date; behaviour changes, so each claim is dated where the source carries a date.

**Source tiers used below:**

- **T1** — vendor documentation, vendor engineering blogs, vendor release notes.
- **T2** — credible independent technical write-ups, reverse-engineering studies, reputable press.
- **T3** — dated user reports, forum and HN threads. Used only for behaviour that is real but undocumented.

Two T1 pages (Superhuman's help centre, one Figma forum thread) returned HTTP 403 to direct fetch; their text is quoted here as returned by the search index and is flagged in place.

---

## Comparison table

| Product | Scoping policy — what is available offline | Opt-in / automatic / total | Eviction | Refused offline | How offline state is surfaced |
|---|---|---|---|---|---|
| **Notion** | Forest of "offline page trees": pages you toggled, plus (paid plans) recents + favourites, plus inherited children of downloaded databases | **Hybrid**: manual toggle for everyone, automatic recents/favourites on Plus/Business/Enterprise | Yes, implicit — a page that falls out of Recents or drops down Favourites can silently stop being offline-available (T2). No documented size cap | Embeds, AI blocks, forms, buttons, sharing, permission editing; database rows past the first 50 of the first view; sub-pages not explicitly marked | Dedicated `Settings → Offline` dashboard listing pages downloaded *by you* vs *by Notion*; per-page progress bar on download |
| **Linear** | Whole permitted workspace replicated to IndexedDB, filtered by sync groups (your user id, your teams, roles); some models `lazy` / `partial` / `explicitlyRequested` and fetched on demand | **Automatic**, near-total within your permission scope. No user control | No user-facing eviction; delta sync via checkpoint, with fallback paths server-side | Anything requiring a model class that was never loaded; Linear's own docs call offline "a failsafe and not a full-fledged feature" | "Syncing" label next to workspace name in sidebar with a count of pending changes |
| **Figma (editor)** | Only what is *currently open*: the current page of open files plus other pages of those files loaded while online | **Implicit working set**, scoped to open tabs. No opt-in, no marking | Browser storage retention: 30 days (Chrome/Firefox/Edge/Opera), 7 days (Safari) | Opening any file not already open; unloaded pages of open files; library components; version history; all multiplayer | Toolbar icons: one for "attempting to save your changes", one for "Figma considers you offline", with tooltip |
| **Figma (prototype presenting)** | One prototype, explicitly preloaded, valid while the tab stays open | **Explicit opt-in**, per artefact | Tab close = gone; must preload again | Mobile entirely; prototypes exceeding browser download limits | Header shows "Available to present while offline" with a download-success icon |
| **Obsidian** | Entire vault, always, on every device. Sync is a backup/propagation layer over local files | **Total local by default**; selective sync is *opt-out* per device | None | Nothing in the notes themselves; only Sync itself pauses | No offline mode to communicate — the local vault is the app |
| **Apple Notes (iCloud)** | All notes present on device; iCloud is a sync channel, not off-device storage (T3) | **Total local**, automatic | None documented | Nothing for note text | No offline affordance; offline is the unremarkable case |
| **Superhuman Mail** | "any message you've opened, searched for, or received in the last 30 days", plus up to 1250 emails per Split, plus attachments | **Automatic implicit working set** | Implicit: the 30-day window and the 1250/Split cap | Nothing prominent — triage, reply, schedule all work; sends queue | "Connecting…" toast on reconnect with a count of emails syncing |
| **Google Docs / Drive** | Hybrid: "some of your most recent files will be automatically saved offline", plus per-file `File → Make available offline` | **Hybrid**, same shape as Notion | Implicit recency; documented dependency on available device space | Files too large to sync offline; browser must be Chrome/Edge with the extension | Per-file document-status icon; Drive shows offline availability state per file |
| **Dropbox** | Explicit per-file/folder "Make available offline"; everything else is online-only placeholder | **Explicit opt-in**, with online-only as the default state | Yes, and it is *administrator-configured*: "Save hard drive space automatically" turns files a member has not opened in a few months back to online-only | Opening an online-only file | Per-file sync-state badge in the file manager (online-only vs available offline) |

---

## Per-product findings

### Notion — the clearest hybrid, and the best-documented policy

Notion shipped real offline mode in **2.53 on 2025-08-19** (T1) after years of promises. TechCrunch covered it on 2025-08-20 as "finally … works without an internet connection" (T2).

The policy, per Notion's own help pages (T1):

- Any user can open a page → `•••` → **Available offline**, and gets a progress bar.
- On Plus/Business/Enterprise, Notion *also* auto-downloads "recently visited and favorited pages".
- "Subpages of any downloaded pages won't automatically download for offline use."
- Databases: "the first 50 rows of the first view" download; further rows must be downloaded individually.
- Unavailable offline: embeds, AI blocks, forms, buttons, page sharing, permission editing.
- `Settings → Offline` is a real dashboard: browse and search downloaded pages, filter by *downloaded by you* vs *downloaded by Notion*, and turn automatic downloads off.

The engineering post **"How we made Notion available offline" (2025-12-11, T1)** is the most useful architectural source in this whole survey. It describes an `offline_page` table plus an `offline_action` table that records *why* each page is offline — toggled, auto-downloaded, inherited — so a page has multiple reasons and is dropped only when the last reason disappears. Notion explicitly rejected total replication: their push-based update model "wouldn't scale to millions of users and devices" if every client held everything.

The counts (**top 20 recents, top 20 favourites**) come from Thomas Frank's guide (August 2025, T2) and are repeated by several SEO pages; **I could not confirm the number 20 in any Notion-owned page.** Frank also documents the eviction surprise directly: "In the event that a page drops out of Recents, or is moved to a much lower spot in your Favorites list, it may not be automatically available offline anymore" — which is why he recommends manual toggling as the safe path. He also reports database auto-caching behaving inconsistently in testing.

Marketing-vs-reality: Notion's marketing says offline works; user reviews collected through 2025–2026 (T3, Capterra and similar, low quality individually) continue to report weak database offline behaviour and unreliable sync of offline edits. The gap is narrower than it was pre-2.53 but is not closed.

### Linear — near-total replication, and a company that tells you not to rely on it

Linear is the app everyone cites, and its own documentation is the most deflationary source in this survey. Linear Docs (T1): **"Offline mode is designed as a failsafe and not a full-fledged feature."** The same page warns that Linear "does not check the creation date of each change before updating data", so a long offline session can overwrite a colleague's edits. The only UI is the word "Syncing" next to the workspace name with a count of pending changes.

Architecturally it *is* near-total replication within a permission boundary. From **"Rebuilding Linear's delta sync read path" (2026-08-18, T1)**: each client maintains a local database and replays an ordered log of sync actions; "sync groups encode access to parts of a workspace, and sync subscriptions narrow that further to the models and views the client currently needs"; clients reconnect with a checkpoint rather than re-bootstrapping. The scale numbers are the interesting part — over 20 TB of sync actions in aggregate, single workspaces producing close to a million sync actions per day, returning clients hundreds of thousands of actions behind.

The reverse-engineering study **wzhudev/reverse-linear-sync-engine (T2)** fills in the client model: three bootstrap modes (full, partial, local) and per-model load strategies — `instant` (loaded at bootstrap, the default), `lazy` (fetched all at once when needed), `partial` ("loaded on demand, meaning only a subset of instances is fetched"), `explicitlyRequested`, and `local`. Related models load via *partial indexes* (e.g. comments by issue id), and the client checks whether a partial index is already local before hitting the network. So: issues are total; comments and heavier associations are not.

Reality check (T3): an HN thread from **November 2022** ("I love linear but your advertised 'offline mode' is not what I…") reports that launching the Mac app while offline gave a blank window or "Unknown Error loading your workspace data" — the user asked only for read-only access to the last synced state. A Linear cofounder replied that the app was built offline-first from the start. Both things were true at once: the data was local, the *app shell and session* were not resilient. This is the single most transferable failure mode in the survey.

### Figma — the strictest working set in the sample, from the app with the largest documents

Figma's help centre, "What can I do offline in Figma?" (T1, undated), is unambiguous. Offline you can edit "the current page of your open files, as well as any additional pages in those files that were loaded while online", create one new file, create layers, change layer properties, use components created *in the current file*, play preloaded prototypes, run plugins that don't need external browser APIs, and save `.fig` to disk. You cannot open files created while online, cannot reach pages of open files that never loaded, cannot use libraries, version history, or any multiplayer feature.

Retention is spelled out per browser: 30 days on Chrome 58+, Firefox 51+, Edge 18+, Opera 45+; **7 days on Safari 10.1+**. That is an eviction policy inherited from the storage layer, not designed — and it is dated by browser vendor, not by Figma.

Offline state is surfaced with two toolbar icons: saving-in-progress, and "Figma considers you offline" with a tooltip.

Forum threads (T3) show users repeatedly discovering that the *desktop app* buys them nothing extra here — "is the figma desktop app no longer available offline?" — because the scoping rule is about what was loaded, not about which client you use.

The instructive twist: Figma **does** ship an explicit per-artefact opt-in, just not in the editor. "Present prototypes offline" (T1) has you click Present → Advanced settings → **Make available offline**, wait for a confirmation, and then the header reads "Available to present while offline". Available on any plan, desktop only, and the preload dies when the tab closes. A company with an otherwise purely implicit model added an explicit opt-in for the one job where being caught without the artefact is catastrophic — presenting to a room.

### Obsidian and Apple Notes — the total-local baseline

Obsidian (T1): a local vault is "the copy of your vault that exists on each of your devices"; Sync adds a remote vault as a hub. Offline is not a mode, it is the default state; edits queue and "sync automatically when your device reconnects". Selective sync inverts the usual polarity — it is *opt-out*, and by default it already **excludes images, audio, video and PDFs** unless you enable "Sync all other types". Two sharp edges: the setting is device-specific, and "adding a file to the Excluded files list does not remove it from the remote vault if it has already been synced" — i.e. exclusion is not retroactive.

Apple Notes (T3, Apple discussion threads): notes synced via iCloud are present on the device itself and readable offline; iCloud is a sync channel, not off-device storage. Users wanting truly local-only notes use the "On My iPhone" account.

Both are bounded by the same thing: the corpus is plain text, one user, megabytes not gigabytes. Nobody has to decide anything because the whole dataset fits.

### Superhuman — automatic implicit working set, with published numbers

Superhuman's help centre article "Offline Access" (T1; direct fetch returned 403, text below is as returned by the search index and should be re-verified before being quoted externally): "Superhuman Mail caches any message you've opened, searched for, or received in the last 30 days. Up to 1250 emails from each Split are also stored and attachments are automatically downloaded."

Offline you triage and reply normally; scheduled sends fire on reconnect. On reconnect the UI shows a "Connecting…" notification bottom-right with a count of emails syncing to the device.

This is the cleanest example of pattern 3 done well: a stated, comprehensible rule (recency window + per-view cap), no user decisions, and no visible eviction event because email is inherently recency-ordered — the thing that ages out is the thing you stopped caring about.

### Google Docs / Drive — the same hybrid as Notion, shipping since long before it was fashionable

Google Docs Editors Help (T1): turn on the Drive **Offline setting**, and "some of your most recent files will be automatically saved offline" if there is space; separately, `File → Make available offline` or the right-click **Available offline** marks a specific document. Requires Chrome or Edge with the Google Docs Offline extension — no offline in other browsers, no offline in incognito. Very large files may fail to sync offline.

Notable for the argument below: this is a fifteen-year-old design, still the shipping design in 2026, and its shape is *exactly* the shape Notion arrived at independently in 2025.

### Dropbox — explicit opt-in with a real, automated eviction policy

Dropbox (T1) is the purest surviving explicit-opt-in system. Files are online-only placeholders by default; "Make available offline" on a file or folder materialises it locally. Selective sync operates on folders only, not individual files. And crucially, admins can enable **"Save hard drive space automatically"**, which reverts files a team member has not opened in a few months to online-only — an eviction policy that overrides the user's earlier explicit choice. Per-file state is visible as a badge in the OS file manager.

---

## The analysis

### Which pattern dominates

Counting the sample: two total (Obsidian, Apple Notes), two implicit working set (Figma editor, Superhuman), one near-total-with-lazy-tail (Linear), three hybrid explicit-plus-automatic (Notion, Google Docs, Dropbox), one pure explicit per-artefact (Figma prototypes).

**No product in the sample is purely explicit opt-in with nothing automatic behind it.** That is the finding that matters most. Everywhere explicit opt-in survives, it is a *supplement* to an automatic layer, not a replacement for one — Notion's toggle sits on top of recents+favourites, Google's `Make available offline` sits on top of "most recent files", Dropbox's per-file materialisation sits underneath an admin auto-eviction sweep. The only pure-explicit case is Figma prototype presenting, which is scoped to a single artefact and a single high-stakes moment.

The dominant *shape* in 2026 is therefore: **automatic implicit working set as the floor, explicit opt-in as the escape hatch, with the app telling you which is which.** Notion's `offline_action` table — recording the *reason* a page is offline, and dropping it only when the last reason expires — is the reference implementation of that shape, and it is nine months old at time of writing.

### What predicts which pattern a product picks

Three variables explain almost every row:

1. **Total corpus size relative to device storage.** This dominates. Obsidian and Apple Notes replicate everything because a text corpus is megabytes. Linear replicates near-everything because issue metadata is small and its scaling problem is the *log*, not the *snapshot* — and even Linear degrades comments to partial-index loading. Figma cannot replicate anything because a single design file can be hundreds of megabytes. Notion sits between and had to invent a policy. **If your dataset fits, you do not need a scoping policy at all, and any UI you add for one is pure cost.**

2. **Per-document size relative to corpus size.** This is subtler and it predicts *implicit vs explicit*. Where documents are small and numerous (notes, issues, emails), recency is a good proxy and implicit works. Where documents are large and few (Figma files, big spreadsheets), the cost of a wrong guess by the system is enormous — gigabytes downloaded for nothing, or the one file you needed absent — so the user's intent becomes worth asking for. Figma's prototype opt-in exists precisely because the artefact is huge and the need is known in advance.

3. **Whether the offline session is *anticipated*.** Recency-based caching answers "the network dropped unexpectedly". Explicit marking answers "I am about to board a plane / go to a site with no signal". These are different products. Every explicit-opt-in survivor in this sample is answering the second question. Dropbox's mobile "available offline", Figma's prototype preload, Notion's toggle before travel — all of them are anticipation UIs.

Collaboration model predicts surprisingly little about *scoping* — but it predicts a lot about what is refused. Every product in the sample refuses the same class of things offline: presence, permissions, sharing, library resolution, anything requiring a server-side authority. Nobody tries to do permissions offline.

Platform matters mainly as a gate, not a policy: Notion and Figma both refuse offline in the browser at all (Notion: desktop/mobile apps only; Figma: Chrome/Edge storage retention windows). Google Docs offline is Chrome/Edge-only in 2026.

### Verdict on explicit per-document opt-in

**Explicit opt-in is not dead, is not a legacy wart, and is not considered unacceptable UX in 2026.** The evidence is direct: Notion designed a brand-new offline system in 2025 and made an explicit per-page toggle the primary, universal mechanism — the automatic layer is the *paid* addition, not the reverse. Figma, whose editor has no opt-in at all, chose to add one for prototype presenting. Dropbox has never removed it. Google has shipped it continuously for over a decade.

What *is* considered unacceptable is **explicit opt-in as the only mechanism, with no automatic safety net and no visibility.** The failure mode users complain about is uniform across the sample: *I forgot to mark it, and I found out on the plane.* Every credible product mitigates this the same two ways — an automatic layer that catches the common case without being asked, and a dedicated surface (Notion's `Settings → Offline`, Dropbox's per-file badges, Figma's "Available to present while offline") that answers "what do I actually have?" **before** the network goes away. Notion's inheritance rule (children of a downloaded database come along, up to 50 rows) is a third mitigation: opt-in at a coarser granularity than the individual page.

Now the honest case against the specific design in question.

The opt-in half of "take a copy offline" is well supported by the evidence. **The fork half is not, and the evidence points against it.** In every product surveyed — including all three explicit-opt-in ones — marking a document for offline creates *the same document, additionally present here*. It keeps its identity, it keeps its URL, and it merges back without the user doing anything. Not one product in this sample mints a divergent copy on an offline request. The closest analogue is Figma's `.fig` export while offline, which Figma's own help describes as saving "a copy of the file as it currently exists" — and notably it is framed as *export*, a different verb, in a different menu, with no expectation of return.

That gap is a product risk, not a technical one. Users have fifteen years of trained expectation that "make available offline" is a *materialisation* control, not an *identity* control. A control that reads like Dropbox's and behaves like `git clone` will be misread, and the misreading is expensive and silent: someone works a full day in the fork believing they are editing the shared sheet. If the design keeps the fork, the language must break the analogy hard — "make a copy I'll merge myself", not "take a copy offline" — and the fork must be visually distinct at every moment of editing, not just at creation.

There is also a second-order argument against. Explicit opt-in survives because it is paired with an automatic floor. A fork cannot serve as that floor: you cannot auto-fork someone's twenty recent documents. So the design as stated has an explicit layer and no floor, which is precisely the configuration users complain about. The strongest version of the design keeps the fork for the deliberate "I am going somewhere without signal and I will reconcile later" case, and adds a separate, non-forking, automatic read-cache for the unanticipated-network-drop case — because those are two different products, and every survivor in this sample ships both.

One genuine point in the fork's favour: Linear's own docs concede that automatic merge-back after a long offline session **can silently overwrite a colleague's work**, because Linear does not compare change timestamps. That is the cost of the "same identity, merges by itself" model stated by its most celebrated practitioner, in its own documentation. A fork with explicit reconciliation refuses to make that trade. For a spreadsheet — where a single overwritten column is worse than a merge conflict, and where structural edits like inserting rows make blind last-write-wins actively dangerous — that refusal is defensible in a way it would not be for a to-do list. The fork is not defensible as *offline scoping*; it is defensible as *conflict policy*, and it should be argued on those grounds.

---

## What I could not establish

- **Notion's "20 recents / 20 favourites" figure.** Reported by Thomas Frank (August 2025) and repeated widely, but absent from every Notion-owned page I fetched. Notion's own docs say only "recent and favorited pages". Treat 20 as unconfirmed.
- **Notion's storage ceiling and eviction rule for *manually* toggled pages.** Neither the help centre nor the December 2025 engineering post states a cache size cap or what happens when a device fills up. The `offline_action` model implies manual toggles never expire on their own, but this is inference, not documentation.
- **Whether Notion's offline behaviour changed between the December 2025 engineering post and September 2026.** I found no Notion release note after 2.53 that revisits scoping.
- **Linear's client-side storage ceiling.** No public figure for how large a workspace's local IndexedDB gets, at what point the client degrades, or whether any eviction exists. The 2026-08-18 post is about the *server* read path.
- **Whether Linear can now cold-start fully offline.** The 2022 HN report says no; Linear's docs are silent; I found no dated report either way from 2025–2026.
- **Superhuman's article verbatim.** help.superhuman.com returned 403 to direct fetch; the quoted text came via the search index. The 30-day / 1250-per-Split figures should be re-verified in a browser before being relied on.
- **Height.** No public documentation of a sync or offline model exists that I could find; searches return generic offline-first framework content. Excluded rather than padded. Google Docs/Drive and Dropbox substitute as documented sync-engine-forward products.
- **Whether any product ships a forked/divergent offline copy.** I found none in this sample and no counter-example elsewhere. Absence of evidence, but the search was directed and came back empty.

---

## Sources

**Tier 1 — vendor**

- Figma, "What can I do offline in Figma?" — https://help.figma.com/hc/en-us/articles/360040328553-What-can-I-do-offline-in-Figma
- Figma, "Present prototypes offline" — https://help.figma.com/hc/en-us/articles/26463081577367-Present-prototypes-offline
- Notion, "Use Notion pages offline" — https://www.notion.com/help/use-pages-offline
- Notion, "Everything you need to know about working offline in Notion" — https://www.notion.com/help/guides/working-offline-in-notion-everything-you-need-to-know
- Notion, Release 2.53 (2025-08-19) — https://www.notion.com/releases/2025-08-19
- Notion, "How we made Notion available offline" (2025-12-11) — https://www.notion.com/blog/how-we-made-notion-available-offline
- Linear Docs, "Download Linear" (offline mode section) — https://linear.app/docs/get-the-app
- Linear, "Scaling the Linear Sync Engine" (2023-06-29) — https://linear.app/now/scaling-the-linear-sync-engine
- Linear, "Rebuilding Linear's delta sync read path" (2026-08-18) — https://linear.app/now/rebuilding-delta-sync-read-path
- Obsidian Help, "Local and remote vaults" — https://obsidian.md/help/sync/vault-types
- Obsidian Help, "Sync settings and selective syncing" — https://obsidian.md/help/sync/settings
- Superhuman Help, "Offline Access" (403 on direct fetch; text via search index) — https://help.superhuman.com/hc/en-us/articles/38458300297875-Offline-Access
- Google Docs Editors Help, "Work on Google Docs, Sheets, & Slides offline" — https://support.google.com/docs/answer/6388102
- Google Drive Help, "Use Google Drive files offline" — https://support.google.com/drive/answer/2375012
- Dropbox Help, "How to make a file or folder available offline" — https://help.dropbox.com/sync/access-files-offline
- Dropbox Help, "Free Up Space with Online-Only Files" — https://help.dropbox.com/sync/make-files-online-only
- Dropbox Help, "How to make your team's files online-only to manage hard drive space" — https://help.dropbox.com/sync/admins-online-only-settings
- Dropbox Help, "Selective sync overview" — https://help.dropbox.com/sync/selective-sync-overview

**Tier 2 — independent technical / press**

- wzhudev, "reverse-linear-sync-engine" — https://github.com/wzhudev/reverse-linear-sync-engine
- Thomas Frank, "Notion Offline Mode: The Ultimate Guide" (August 2025) — https://thomasjfrank.com/notion-offline-mode-the-ultimate-guide/
- TechCrunch, "Finally, Notion now works without an internet connection" (2025-08-20) — https://techcrunch.com/2025/08/20/finally-notion-now-works-without-an-internet-connection/
- XDA, "Notion's offline mode is finally here, but it comes with a strange limitation" (2025-08-20) — https://www.xda-developers.com/notion-offline-mode-launched/
- bytemash.net, "Linear sent me down a local-first rabbit hole" (2025-08-05) — https://bytemash.net/posts/i-went-down-the-linear-rabbit-hole/

**Tier 3 — dated user reports**

- Hacker News, "I love linear but your advertised 'offline mode' is not what I…" (November 2022), incl. a Linear cofounder reply — https://news.ycombinator.com/item?id=33587398
- Hacker News, "Linear sent me down a local-first rabbit hole" discussion (2025-08-08) — https://news.ycombinator.com/item?id=44833834
- Figma Forum, "Is the figma desktop app no longer available offline?" — https://forum.figma.com/t/is-the-figma-desktop-app-no-longer-available-offline/50116
- Apple Community, "Why are Notes in iCloud available offline?" — https://discussions.apple.com/thread/252707775
