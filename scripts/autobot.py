#!/usr/bin/env python3
"""AutoBot：挑一条上游改动 → 适配到本仓 → 跑测试 → 提 PR 给人审。

两条实施路径，同一道测试闸：
  1. git cherry-pick -x —— 原样移植。最便宜，能落就落。
  2. LLM 适配 —— cherry-pick 冲突时，把上游 diff + 本仓现有文件喂给模型，让它改写成本仓
     能用的补丁。本仓多数改动是「改写实现」，原样移植必然冲突，这条路才是常态。
     补丁由 git apply --3way 落地，能不能留仍由 go test 判定。

安全线（AutoBot.yml 的 never 在这里落地）：不推 main、不合并、不打 tag、不发版，
不改 workflows/AGENTS.md/config.example.json。任何一步失败只出报告、不开 PR。

用法：
    python3 scripts/autobot.py                     # 用 AutoBot.yml 里的上游
    python3 scripts/autobot.py --repo owner/name   # 临时看别的仓库（可给完整 URL）
    python3 scripts/autobot.py --dry-run           # 只列候选与判定，不落地不推送
"""

import argparse
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request

import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPORT = os.path.join(ROOT, "autobot-report.md")
FORBIDDEN = {".github/workflows/", "AGENTS.md", "config.example.json"}
PATCH = "/tmp/autobot.patch"


def run(*args, check=True):
    """跑命令。check=False 时返回 (code, 输出)。"""
    p = subprocess.run(args, cwd=ROOT, capture_output=True, text=True)
    if check and p.returncode != 0:
        raise RuntimeError(f"{' '.join(args)} 失败: {p.stderr.strip()[:400]}")
    return p.stdout.strip() if check else (p.returncode, (p.stdout + p.stderr).strip())


def say(lines, text=""):
    lines.append(text)
    print(text)


def llm(base, key, model, prompt):
    """一次 OpenAI 兼容调用。失败抛异常（调用方降级成只出报告）。"""
    body = json.dumps({"model": model, "temperature": 0,
                       "messages": [{"role": "user", "content": prompt}]}).encode()
    req = urllib.request.Request(
        base.rstrip("/") + "/chat/completions", data=body,
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=300) as r:
            return json.loads(r.read())["choices"][0]["message"]["content"]
    except urllib.error.HTTPError as e:
        # 只留 "HTTP Error 400" 等于没报错：网关的错误体（duck.ai 的 ERR_BAD_REQUEST
        # 之类）才是可行动的信息，截 400 字节带出去。
        raise RuntimeError(f"HTTP {e.code}: " + e.read()[:400].decode("utf-8", "replace")) from None


def pick_json(text):
    """从模型回复里抠出第一个 JSON 对象（有些模型会裹 ```json 围栏）。"""
    m = re.search(r"\{.*\}", text, re.S)
    if not m:
        raise ValueError("模型回复里没有 JSON")
    return json.loads(m.group(0))


def strip_fence(text):
    """去掉 ```diff / ``` 围栏，返回纯补丁文本。"""
    m = re.search(r"```(?:diff|patch)?\n(.*?)```", text, re.S)
    return (m.group(1) if m else text).strip() + "\n"


def verdict_of(out):
    """解析模型的四档判定：返回 (verdict, why, patch 正文)。解析不出来按 skip 处理。"""
    try:
        d = pick_json(out)
    except Exception:  # noqa: BLE001
        return "skip", "回复不是 JSON：" + out[:120].replace("\n", " "), ""
    v = str(d.get("verdict") or "skip").strip().lower()
    why = str(d.get("why") or "").strip()
    body = strip_fence(str(d.get("patch") or "")).strip()
    return v, why or "（模型未给原因）", (body + "\n") if body else ""


def adapt(sha, base, branch, base_url, key, model, lines):
    """让模型把上游提交改写成本仓能用的补丁，提交到 branch。返回是否成功。"""
    run("git", "checkout", "-q", "-B", branch, base)
    diff = run("git", "show", "--format=", sha)
    files = run("git", "show", "--name-only", "--format=", sha).split()
    excerpts = []
    for f in files[:3]:
        fp = os.path.join(ROOT, f)
        if os.path.isfile(fp):
            excerpts.append(f"--- 本仓现有 {f}（节选） ---\n" + open(fp, encoding="utf-8").read()[:2500])
        else:
            excerpts.append(f"--- {f} 在本仓不存在（新文件，或本仓实现在别处） ---")

    def build(diff_cap, ex_cap):
        return ("下面是一个上游提交的 diff。本仓是它的长期 fork，同一功能往往已有**另一种实现**，"
                "所以原样移植必然冲突——这正是要你做的事：判断本仓现状，再只补缺的那部分。\n\n"
                "输出**一个 JSON 对象**，不要别的话：\n"
                "{\"verdict\": \"adapt|partial|already_have|skip\",\n"
                " \"why\": \"一到两句：本仓现状是什么、依据是什么\",\n"
                " \"patch\": \"unified diff（仅 adapt/partial 时给；否则空串）\"}\n\n"
                "四档含义：\n"
                "- adapt：整条都能搬，改写成本仓风格；\n"
                "- partial：本仓已实现一部分（说清是哪部分），patch **只包含缺口**——这是常态，"
                "上游常把多个修复塞进一条提交；\n"
                "- already_have：目的已由本仓其他代码达成，说清在哪个文件/函数；\n"
                "- skip：依赖本仓没有的抽象，或改动对本仓无意义，说清缺什么。\n"
                "patch 必须是 `git apply --3way` 能直接用的 unified diff（含 diff --git / --- / +++ / @@）。\n\n"
                f"===== 上游 diff（{sha}）=====\n{diff[:diff_cap]}\n\n" + "\n\n".join(ex_cap))

    # 网关（duck.ai 系）对超长 prompt 直接 400，且不告诉你为什么：先按常规尺寸试，
    # 失败了就砍到只剩 diff 再来一次。两次都失败才放弃这条候选。
    out = None
    for i, (cap, ex) in enumerate(((30000, excerpts), (6000, []))):
        try:
            out = llm(base_url, key, model, build(cap, ex))
            break
        except Exception as e:  # noqa: BLE001
            say(lines, f"  {sha}: 第 {i + 1} 次适配调用失败：{e}")
    if out is None:
        return False
    verdict, why, body = verdict_of(out)
    say(lines, f"  {sha}: 判定={verdict} — {why}")
    if verdict in ("skip", "already_have") or not body.strip():
        return False
    with open(PATCH, "w", encoding="utf-8") as f:
        f.write(body)
    code, msg = run("git", "apply", "--3way", PATCH, check=False)
    if code != 0:
        # 模型给的 diff 常在 hunk 头上写错行数（「corrupt patch」），--recount 让 git 重算；
        # 真冲突（context 不匹配）它救不了，那就是摘要节选不够，得换人看。
        code, msg = run("git", "apply", "--recount", "-C1", PATCH, check=False)
        if code != 0:
            say(lines, f"  {sha}: 补丁应用失败：\n```\n" + msg[:600] + "\n```")
            return False
    code, msg = run("go", "test", "./...", check=False)
    if code != 0:
        run("git", "checkout", "-q", "--", ".")
        run("git", "clean", "-qfd")
        say(lines, f"  {sha}: 适配后测试未通过：\n```\n" + msg[-1200:] + "\n```")
        return False
    run("git", "add", "-A")
    run("git", "commit", "-q", "-m", f"autobot: 适配上游 {sha}（LLM 改写，非原样移植）")
    return True


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", help="临时指定上游仓库（owner/name 或 URL），默认取 AutoBot.yml")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    cfg = yaml.safe_load(open(os.path.join(ROOT, "AutoBot.yml"), encoding="utf-8"))
    up, pol = cfg["upstream"], cfg["policy"]
    src = (args.repo or up["repo"]).replace("https://github.com/", "").replace(".git", "").strip("/")
    lines = ["# AutoBot 报告", ""]

    # 1) 拉上游：--repo 可临时换别家，走同一个 src remote
    run("git", "remote", "remove", "src", check=False)
    run("git", "remote", "add", "src", f"https://github.com/{src}.git")
    code, msg = run("git", "fetch", "-q", "src", "main", check=False)
    if code != 0:
        say(lines, f"拉取 {src} 失败：{msg[:300]}")
        write_report(lines)
        return 0

    # 2) 候选：最近 N 条非 merge、非 chore 提交
    log = run("git", "log", f"--max-count={up['limit']}", "--no-merges",
              "--format=%h%x09%ad%x09%s", "--date=short", "src/main")
    cands = [l.split("\t", 2) for l in log.splitlines()
             if l.strip() and not re.match(r"\s*chore", l.split("\t", 2)[2])]
    # git cherry 用 patch-id 挡掉「逐字节等同」的移植；改写实现的挡不住，交给适配路径。
    applied = {l[2:9] for l in run("git", "cherry", "-v", "HEAD", "src/main").splitlines()
               if l.startswith("- ")}
    cands = [c for c in cands if c[0] not in applied]
    tried = set(run("git", "ls-remote", "--heads", "origin", pol["branch_prefix"] + "*").split())
    cands = [c for c in cands if f"refs/heads/{pol['branch_prefix']}{c[0]}" not in tried]
    if not cands:
        say(lines, f"{src}：没有未处理的新提交（已去 merge/chore/已吸收/已试过）。")
        write_report(lines)
        return 0
    say(lines, f"{src}：候选 {len(cands)} 条（最近 {up['limit']} 条上游提交，已去 merge/chore/已吸收）。")

    listing = "\n".join(
        f"- {sha} {date} {subj} | " + run("git", "show", "--stat=0", "--name-only",
                                          "--format=", sha).replace("\n", ",")
        for sha, date, subj in cands[:40])
    key = os.environ.get("LLM_API_KEY", "")
    base_url = os.environ.get("LLM_BASE_URL", "")
    model = os.environ.get("LLM_MODEL", "")

    # 3) 没 key 就只出清单（本地跑与首次接入时的正常降级）
    if not (key and base_url and model):
        say(lines, "未配置 LLM_API_KEY / LLM_BASE_URL / LLM_MODEL：只出候选清单，不开 PR。")
        say(lines)
        say(lines, listing)
        write_report(lines)
        return 0

    # 4) 让模型排序：挑哪条、为什么、风险、置信度
    prompt = (
        "你在给一个 fork 挑选值得吸收的上游提交。本仓是 workbuddy2api-panel 的长期 fork，"
        "缓存键、补偿、检索、签到、面板、调度都有自有实现。\n"
        "按值得程度挑**最多 3 条**：优先真 bug 修复、改动小、目的清晰。输出 JSON："
        "{\"picks\":[{\"sha\":\"...\",\"why\":\"一句话\",\"risk\":\"一句话\",\"confidence\":0-3}]}。"
        "没有值得的就 {\"picks\":[]}。\n\n" + listing)
    try:
        picks = [p for p in (pick_json(llm(base_url, key, model, prompt)).get("picks") or [])
                 if p.get("sha")]
    except Exception as e:  # noqa: BLE001 —— LLM 侧任何失败都降级成报告
        say(lines, f"LLM 调用失败，本次不开 PR：{e}")
        write_report(lines)
        return 0
    if not picks:
        say(lines, "模型判定没有值得吸收的候选。")
        write_report(lines)
        return 0
    say(lines, "模型排序：" + " · ".join(f"{p['sha']}({p.get('confidence')})" for p in picks))

    # 5) 逐条尝试：cherry-pick 失败 → 交模型适配 → 仍由 go test 裁决
    known = {c[0] for c in cands}
    for pick in picks[:3]:
        sha = pick["sha"].strip()
        if sha not in known or int(pick.get("confidence") or 0) < 2:
            say(lines, f"{sha}: 不在候选里或置信不足，跳过。")
            continue
        hits = [f for f in run("git", "show", "--name-only", "--format=", sha).split()
                for bad in FORBIDDEN if f.startswith(bad)]
        if hits:
            say(lines, f"{sha}: 改了禁区文件 {sorted(set(hits))}，跳过。")
            continue
        if args.dry_run:
            say(lines, f"{sha}: dry-run 命中（{pick.get('why', '')}），不落地。")
            write_report(lines)
            return 0
        branch = pol["branch_prefix"] + sha
        run("git", "checkout", "-q", "-B", branch, pol["base"])
        code, out = run("git", "cherry-pick", "-x", sha, check=False)
        how = "cherry-pick"
        if code != 0:
            run("git", "cherry-pick", "--abort", check=False)
            say(lines, f"{sha}: 原样移植冲突，改试 LLM 适配。")
            if not adapt(sha, pol["base"], branch, base_url, key, model, lines):
                continue
            how = "LLM 适配"
        else:
            code, out = run("go", "test", "./...", check=False)
            if code != 0:
                run("git", "checkout", "-q", pol["base"])
                say(lines, f"{sha}: 移植后测试未通过，换下一条。\n```\n" + out[-1200:] + "\n```")
                continue
        run("git", "push", "-q", "-u", "origin", branch)
        run("gh", "label", "create", pol["label"], "--repo", os.environ["GITHUB_REPOSITORY"],
            "--color", "ededed", "--description", "AutoBot 自动挑选的上游改动", check=False)
        title = f"autobot: 吸收 {src}@{sha} — {pick.get('why', '')}"[:120]
        body = (f"来源 `{src}@{sha}`，实施方式：**{how}**。\n\n"
                f"- 理由：{pick.get('why', '')}\n- 风险：{pick.get('risk', '')}\n"
                f"- `go test ./...` 已通过；合并前请人工确认实现口径。\n")
        run("gh", "pr", "create", "--repo", os.environ["GITHUB_REPOSITORY"], "--base", pol["base"],
            "--head", branch, "--title", title, "--body", body, "--label", pol["label"])
        say(lines, f"已开 PR（{how}）：{branch}")
        write_report(lines)
        return 0

    say(lines, "候选都不满足条件，本次不开 PR（只出报告）。")
    write_report(lines)
    return 0


def write_report(lines):
    with open(REPORT, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    sys.exit(main())
