#!/usr/bin/env python3
"""发布后自检：Release 里的 xpi 必须与仓库源码一致。

为什么需要它：插件清单现在有**三份独立副本** ——

    plugin/manifest.json                    仓库源码
    <面板下发的>.xpi 里的 manifest.json      打包产物
    Release 里的 xpi 里的 manifest.json      发布产物

三者本该相同，但没有任何机制保证。而漂移的表现是**静默的**：
update_url 指错 → 自动更新永远查不到；版本号不一致 → 用户装了和清单对不上的包。
两种都不会报错，只会「不工作」。

这个脚本在发布之后从 Release 把 xpi 拉回来，逐字段比对源码。

用法：
    python scripts/verify-release.py --repo leobba/litflow --tag v1.13.0
    python scripts/verify-release.py --repo ... --tag ... --feed-tag release

退出码：0 一致 / 1 有差异或拉不到。
"""
import argparse
import io
import json
import sys
import urllib.request
import zipfile

ROOT_UP = 1  # 从 scripts/ 上一级找源码


def fetch(url):
    req = urllib.request.Request(url, headers={"User-Agent": "litflow-verify"})
    with urllib.request.urlopen(req, timeout=60) as r:
        return r.read()


def source_manifest():
    import os
    here = os.path.dirname(os.path.abspath(__file__))
    path = os.path.join(os.path.dirname(here), "plugin", "manifest.json")
    with io.open(path, encoding="utf-8") as f:
        return json.load(f)


def manifest_from_xpi(raw):
    with zipfile.ZipFile(io.BytesIO(raw)) as z:
        # 顺带校验目录条目：Gecko 没有它就判包无效，而错误信息不会提 zip
        if not any(n.endswith("/") for n in z.namelist()):
            raise SystemExit("  xpi 缺目录条目 —— Zotero 会拒绝安装")
        return json.loads(z.read("manifest.json").decode("utf-8"))


def main():
    # Windows 控制台默认 GBK，打不出 ✓ / ✗。这跟比对逻辑无关，
    # 但会让一个「一致性检查通过」的脚本以非 0 退出，看起来像失败。
    try:
        sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding="utf-8", errors="replace")
        sys.stderr = io.TextIOWrapper(sys.stderr.buffer, encoding="utf-8", errors="replace")
    except Exception:
        pass

    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True)
    ap.add_argument("--tag", required=True)
    ap.add_argument("--feed-tag", default="release")
    a = ap.parse_args()

    src = source_manifest()
    addon = src["applications"]["zotero"]
    addon_id = addon["id"]

    base = f"https://github.com/{a.repo}/releases/download"
    xpi_url = f"{base}/{a.tag}/{addon_id}.xpi"
    feed_url = f"{base}/{a.feed_tag}/update.json"

    problems = []

    # 1. Release 里的 xpi 与源码
    print(f"  比对 xpi  {xpi_url}")
    try:
        raw = fetch(xpi_url)
    except Exception as e:
        problems.append(f"拉不到发布版 xpi: {e}")
        raw = None

    if raw:
        shipped = manifest_from_xpi(raw)
        shipped_app = shipped.get("applications", {}).get("zotero", {}) or {}
        if shipped.get("version") != src["version"]:
            problems.append(
                f"xpi version = {shipped.get('version')!r}，源码是 {src['version']!r}")
        for k in ("id", "update_url", "strict_min_version", "strict_max_version"):
            if shipped_app.get(k) != addon.get(k):
                problems.append(
                    f"xpi {k} = {shipped_app.get(k)!r}，源码是 {addon.get(k)!r}")
        print(f"    版本 {shipped.get('version')}  url={shipped_app.get('update_url')}")

    # 2. 更新清单与源码
    print(f"  比对清单 {feed_url}")
    try:
        feed = json.loads(fetch(feed_url).decode("utf-8"))
    except Exception as e:
        problems.append(f"拉不到更新清单: {e}")
        feed = None

    if feed:
        try:
            upd = feed["addons"][addon_id]["updates"][0]
        except (KeyError, IndexError):
            problems.append(f"更新清单里没有 {addon_id} 的条目")
            upd = None
        if upd:
            if upd.get("version") != src["version"]:
                problems.append(
                    f"清单 version = {upd.get('version')!r}，源码是 {src['version']!r}")
            # update_link 必须指向**真实存在**的资产。这一条是踩过的坑：
            # 清单本身合法、地址也能返回 200，只有真去 HEAD 才能发现 404。
            link = upd.get("update_link", "")
            print(f"    校验 update_link {link}")
            try:
                req = urllib.request.Request(link, method="HEAD",
                                             headers={"User-Agent": "litflow-verify"})
                with urllib.request.urlopen(req, timeout=60) as r:
                    if r.status != 200:
                        problems.append(f"update_link 返回 {r.status}")
            except Exception as e:
                problems.append(f"update_link 不可达: {e}")
            if a.tag not in link:
                problems.append(
                    f"update_link 里的 tag 不是本次发布的 {a.tag}：{link}")

    if problems:
        print()
        for p in problems:
            print(f"  ✗ {p}", file=sys.stderr)
        print()
        print("  三份副本不一致 —— 自动更新会静默失效。", file=sys.stderr)
        return 1

    print("  ✓ 源码 / Release xpi / 更新清单 三者一致，且 update_link 可达")
    return 0


if __name__ == "__main__":
    sys.exit(main())
