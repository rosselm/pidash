#!/usr/bin/env python3
"""Headless check of the running dashboard.

Exists because pidash is a visual program that was, for several commits,
verified only by reading its own source. Bracket-balance checks and API smoke
tests cannot see a wrapped table cell, a card clipping its content, or a
filesystem listed three times.

    python3 -m venv .venv && .venv/bin/pip install playwright
    .venv/bin/python tools/uicheck.py --url http://localhost:8090 --out /tmp/shots

Uses the system Chromium (apt install chromium); Playwright ships no arm64
browser build, so there is nothing to download.
"""
import argparse, json, sys
from playwright.sync_api import sync_playwright

CHECKS = """() => {
  // Measure the text, not the cell: a cell stretches to its row's height, so a
  // tall neighbour (the two-line service description) would otherwise look like
  // wrapping everywhere. A Range over the contents reports one client rect per
  // visual line, which is exactly the question being asked.
  const wrapped = [];
  for (const td of document.querySelectorAll('td')) {
    if (td.querySelector('.sub2')) continue;   // deliberately two lines
    // Per text node: an inline-flex child (the state pill) contributes its own
    // box to a whole-cell range and would read as a second line.
    const walk = document.createTreeWalker(td, NodeFilter.SHOW_TEXT);
    while (walk.nextNode()) {
      const range = document.createRange();
      range.selectNodeContents(walk.currentNode);
      if (range.getClientRects().length > 1) {
        wrapped.push(walk.currentNode.textContent.trim().slice(0, 32));
        break;
      }
    }
  }
  return {
    horizontalOverflow: document.documentElement.scrollWidth > document.documentElement.clientWidth + 1,
    clippedCards: [...document.querySelectorAll('.card')]
      .filter(e => e.scrollHeight > e.clientHeight + 1).map(e => e.dataset.card),
    wrappedCells: wrapped,
    emptyPanels: [...document.querySelectorAll('.card')]
      .filter(e => e.querySelector('.empty')).map(e => e.dataset.card),
    gauges: [...document.querySelectorAll('.gauge-face .val .n')].map(e => e.textContent),
    cores: document.querySelectorAll('#cores .core').length,
    disks: [...document.querySelectorAll('#disks .meter .name')].map(e => e.textContent),
    throttleFlags: document.querySelectorAll('#flags .flag').length,
    pageHeight: Math.ceil(document.body.scrollHeight),
  };
}"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", default="http://localhost:8090/")
    ap.add_argument("--out", default="/tmp/pidash-shots")
    ap.add_argument("--chromium", default="/usr/bin/chromium")
    ap.add_argument("--settle", type=int, default=12000, help="ms to let charts fill")
    args = ap.parse_args()

    problems, console = [], []
    with sync_playwright() as p:
        browser = p.chromium.launch(executable_path=args.chromium,
                                    args=["--no-sandbox", "--disable-dev-shm-usage"])
        ctx = browser.new_context(viewport={"width": 1680, "height": 1100})
        page = ctx.new_page()
        page.on("console", lambda m: console.append(f"{m.type}: {m.text}") if m.type == "error" else None)
        page.on("pageerror", lambda e: console.append(f"pageerror: {e}"))

        page.goto(args.url, wait_until="domcontentloaded")
        page.wait_for_function("document.querySelector('#status').dataset.state === 'up'", timeout=25000)
        page.wait_for_function("document.querySelector('#procs tbody tr')", timeout=25000)
        page.wait_for_timeout(args.settle)

        r = page.evaluate(CHECKS)

        # Size the viewport to the page: a full_page capture puts position:fixed
        # elements (the drawer) somewhere they never actually appear.
        page.set_viewport_size({"width": 1680, "height": min(r["pageHeight"] + 20, 4000)})
        page.wait_for_timeout(1200)
        page.screenshot(path=f"{args.out}/dashboard.png")
        page.keyboard.press("j")
        page.wait_for_timeout(1000)
        if not page.eval_on_selector("#drawer", "e => e.classList.contains('open')"):
            problems.append("drawer did not open on 'j'")
        page.screenshot(path=f"{args.out}/drawer.png")

        for w in (1280, 980, 820, 420):
            page.set_viewport_size({"width": w, "height": 1000})
            page.wait_for_timeout(500)
            if page.evaluate("document.documentElement.scrollWidth > document.documentElement.clientWidth + 1"):
                problems.append(f"horizontal overflow at {w}px")
        browser.close()

    if r["horizontalOverflow"]: problems.append("horizontal overflow at 1680px")
    if r["clippedCards"]:       problems.append(f"cards clipping content: {r['clippedCards']}")
    if r["wrappedCells"]:       problems.append(f"table cells wrapped: {r['wrappedCells']}")
    if r["cores"] == 0:         problems.append("no per-core bars rendered")
    if not r["disks"]:          problems.append("no filesystems rendered")
    if r["throttleFlags"] != 4: problems.append(f"expected 4 throttle flags, got {r['throttleFlags']}")
    if console:                 problems.append(f"console errors: {console}")

    print(json.dumps(r, indent=2))
    print(f"\nscreenshots: {args.out}")
    if problems:
        print("\nFAIL")
        for p_ in problems:
            print(" -", p_)
        return 1
    print("\nOK — no problems found")
    return 0


if __name__ == "__main__":
    sys.exit(main())
