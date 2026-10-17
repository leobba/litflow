# 构建 Zotero 插件 xpi。
#
# 为什么用 Python 而不是 Compress-Archive：Zotero/Gecko 的 nsZipArchive 需要
# **目录条目**才能在 jar: 里解析路径。Compress-Archive 和 .NET 的
# CreateEntryFromFile 都只写文件条目，打出来的包在 Python/7-Zip/资源管理器里
# 完全正常，但 Zotero 会判为无效包，报「它可能无法与该版本的 Zotero 兼容」——
# 一个完全不提 zip 结构的错误。这是本插件装不上四轮的真正原因。
#
# 用法：python scripts/build-plugin.py [--out build]
import argparse
import json
import os
import sys
import zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PLUGIN = os.path.join(ROOT, "plugin")

# 打包清单。顺序会影响 zip 内的物理顺序，manifest.json 放前面便于人工检查。
FILES = [
    "manifest.json",
    "bootstrap.js",
    "prefs.js",
    "update.json",
    "README.md",
    "content/dualsync.js",
    "content/icons/favicon.png",
    "content/icons/favicon@0.5x.png",
]


def read_manifest():
    with open(os.path.join(PLUGIN, "manifest.json"), encoding="utf-8") as f:
        return json.load(f)


def build(out_dir):
    man = read_manifest()
    addon_id = man["applications"]["zotero"]["id"]
    version = man["version"]
    name = f"{addon_id}.xpi"
    os.makedirs(out_dir, exist_ok=True)
    out = os.path.join(out_dir, name)

    added_dirs = set()
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for rel in FILES:
            full = os.path.join(PLUGIN, rel.replace("/", os.sep))
            if not os.path.exists(full):
                print(f"  跳过缺失文件: {rel}", file=sys.stderr)
                continue
            # 先写祖先目录条目（STORED，与方法 0 一致）
            parts = rel.split("/")[:-1]
            for i in range(len(parts)):
                d = "/".join(parts[: i + 1]) + "/"
                if d not in added_dirs:
                    zi = zipfile.ZipInfo(d)
                    zi.external_attr = 0o40755 << 16
                    zi.compress_type = zipfile.ZIP_STORED
                    z.writestr(zi, b"")
                    added_dirs.add(d)
            z.write(full, rel, compress_type=zipfile.ZIP_DEFLATED)

    # 自检：目录条目在不在、CRC 好不好、manifest 能不能读回来
    with zipfile.ZipFile(out) as z:
        names = z.namelist()
        if not any(n.endswith("/") for n in names):
            raise SystemExit("打包结果没有目录条目 —— Zotero 会拒绝安装")
        bad = z.testzip()
        if bad:
            raise SystemExit(f"zip CRC 校验失败: {bad}")
        back = json.loads(z.read("manifest.json").decode("utf-8"))
        if back["version"] != version:
            raise SystemExit("包内 manifest 版本与源文件不一致")

    size = os.path.getsize(out)
    print(f"  xpi   {name}  {size} bytes  v{version}")
    print(f"  目录条目 {sorted(added_dirs)}")
    return out, addon_id, version


def write_update_manifest(out_dir, addon_id, version, repo, tag=""):
    """生成发布到固定 `release` tag 的 update.json。

    update_url 是**写死在已安装用户设备上**的地址，所以它必须指向一个永远不变的
    位置 —— 也就是固定的 `release` tag，而不是每次发版都变的 `v<version>`。
    """
    man = read_manifest()["applications"]["zotero"]
    doc = {
        "addons": {
            addon_id: {
                "updates": [
                    {
                        "version": version,
                        # tag 由调用方给出：xpi 随**网关**的 Release 一起发布，
                        # 所以这里必须用网关的 tag，而不是插件自己的版本号。
                        "update_link": (
                            f"https://github.com/{repo}/releases/download/"
                            f"{tag}/{addon_id}.xpi"
                        ),
                        "applications": {
                            "zotero": {
                                # 11.* 而不是 10.*：版本是逐段比较的，10.0.6 与 10.*
                                # 在第 1 段是「数字 0 vs 字符串 *」，数字更大，严格比较
                                # 下会判为不兼容。11.* 在第 0 段比的是 10 与 11。
                                "strict_min_version": man["strict_min_version"],
                                "strict_max_version": "11.*",
                            }
                        },
                    }
                ]
            }
        }
    }
    p = os.path.join(out_dir, "update.json")
    with open(p, "w", encoding="utf-8", newline="\n") as f:
        json.dump(doc, f, ensure_ascii=False, indent=2)
        f.write("\n")
    print(f"  update.json -> {p}")
    return p


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=os.path.join(ROOT, "build"))
    ap.add_argument("--repo", default="leobba/litflow",
                    help="owner/repo，用于生成 update.json 里的下载地址")
    ap.add_argument("--no-update", action="store_true",
                    help="只打 xpi，不生成 update.json")
    ap.add_argument("--tag", default="",
                    help="xpi 所在 Release 的 tag（默认 v<插件版本>）。"
                         "与网关一起发版时必须传网关的 tag —— xpi 跟着网关的 Release 走，"
                         "清单里若写插件版本就会指向一个不存在的资产，而那种 404 是静默的。")
    a = ap.parse_args()

    xpi, addon_id, version = build(a.out)
    if not a.no_update:
        write_update_manifest(a.out, addon_id, version, a.repo,
                              tag=a.tag.strip() if a.tag else f"v{version}")


if __name__ == "__main__":
    main()
