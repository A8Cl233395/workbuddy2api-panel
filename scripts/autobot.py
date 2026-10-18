#!/usr/bin/env python3
"""AutoBot：每日挑一条上游改动 → 开分支 → 跑测试 → 提 PR 给人审。

职责边界（重要）：LLM 只负责「挑哪一条」，不写代码。代码由 git cherry-pick 从上游搬，
能不能留下由 go test 判定。让模型凭空生成 Go 补丁既不可靠也验不了，所以不做。

安全线（AutoBot.yml 的 never 在这里落地）：不推 main、不合并、不打 tag、不发版，
不改 workflows/AGENTS.md/config.example.json。任何一步失败就只出报告、不开 PR。

用法：
    python3 scripts/autobot.py            # 正常跑（CI 内）
    python3 scripts/autobot.py --dry-run  # 只列候选与判定，不 cherry-pick、不推送
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


def run(*args, check=True, cwd=ROOT):
    """跑命令，返回 stdout（去尾空白）。check=False 时返回 (code, out)。"""
    p = subprocess.run(args, cwd=cwd, capture_output=True, text=True)
    if check and p.returncode != 0:
        raise RuntimeError(f"{' '.join(args)} 失败: {p.stderr.strip()[:400]}")
    return p.stdout.strip() if check else (p.returncode, (p.stdout + p.stderr).strip())


def say(lines, text=""):
    lines.append(text)
    print(text)


def llm(base, key, model, prompt):
    """一次 OpenAI 兼容调用。返回文本；任何失败抛异常（调用方降级成只出报告）。"""
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "temperature": 0,
    }).encode()
    req = urllib.request.Request(
        base.rstrip("/") + "/chat/completions", data=body,
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=180) as r:
        d = json.loads(r.read())
    return d["choices"][0]["message"]["content"]


def pick_json(text):
    """从模型回复里抠出第一个 JSON 对象（有些模型会裹 ```json）。"""
    m = re.search(r"\{.*\}", text, re.S)
    if not m:
        raise ValueError("模型回复里没有 JSON")
    return json.loads(m.group(0))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    cfg = yaml.safe_load(open(os.path.join(ROOT, "AutoBot.yml"), encoding="utf-8"))
    up = cfg["upstream"]
    pol = cfg["policy"]
    lines = ["# AutoBot 报告", ""]

    # 1) 候选：最近 N 天的上游提交（合并提交不收，它没有独立改动）
    log = run("git", "log", f"--max-count={up['limit']}",
              "--no-merges", "--format=%h%x09%ad%x09%s", "--date=short", up["remote"] + "/main")
    cands = [l.split("\t", 2) for l in log.splitlines()
             if l.strip() and not re.match(r"\s*chore", l.split("\t", 2)[2])]
    # 预筛掉「内容上已有等价改动」的提交：git cherry 用 patch-id 比对，标 - 的就是
    # 本仓已经吸收过的。没有这层，模型会把我们早就实现过的修复当成候选（实测第一次
    # 真跑就挑了 535e19e —— 我们早已有 nameBadIDs，cherry-pick 直接冲突）。
    applied = {l[2:9] for l in run("git", "cherry", "-v", "HEAD",
                                   up["remote"] + "/main").splitlines() if l.startswith("- ")}
    cands = [c for c in cands if c[0] not in applied]
    if not cands:
        say(lines, "最近没有新的上游提交。")
        write_report(lines)
        return 0
    say(lines, f"候选 {len(cands)} 条（最近 {up['limit']} 条上游提交，已去合并与 chore）。")

    # 2) 已处理过的就跳过（分支名去重，不维护状态文件）
    tried = set(run("git", "ls-remote", "--heads", "origin", pol["branch_prefix"] + "*").split())
    cands = [c for c in cands if f"refs/heads/{pol['branch_prefix']}{c[0]}" not in tried]
    if not cands:
        say(lines, "候选都已处理过（分支已存在）。")
        write_report(lines)
        return 0

    # 3) 让模型挑一条：只给它提交标题与改动文件，不给全量 diff（省 token）
    listing = "\n".join(
        f"- {sha} {date} {subj} | " + run("git", "show", "--stat=0", "--name-only",
                                          "--format=", sha, cwd=os.path.join(ROOT)).replace("\n", ",")
        for sha, date, subj in cands[:40])
    prompt = (
        "你在给一个 fork 挑选值得吸收的上游提交。本仓是 workbuddy2api-panel 的长期 fork，"
        "已有大量自有实现（缓存键、补偿、检索、签到、面板、调度都有改动）。\n"
        "从下面候选中按值得程度挑**最多 3 条**：优先真 bug 修复、改动小、且不太可能与本"
        "仓已有实现冲突。输出 JSON：{\"picks\":[{\"sha\":\"...\",\"why\":\"一句话\","
        "\"risk\":\"一句话\",\"confidence\":0-3}]}。没有值得的就 {\"picks\":[]}。\n\n" + listing)
    key = os.environ.get("LLM_API_KEY", "")
    base = os.environ.get("LLM_BASE_URL", "")
    model = os.environ.get("LLM_MODEL", "")
    if not (key and base and model):
        say(lines, "未配置 LLM_API_KEY / LLM_BASE_URL / LLM_MODEL：本次只出候选清单，不开 PR。")
        say(lines, "")
        say(lines, listing)
        write_report(lines)
        return 0
    try:
        choice = pick_json(llm(base, key, model, prompt))
    except Exception as e:  # noqa: BLE001 —— 任何 LLM 侧失败都降级
        say(lines, f"LLM 调用失败，本次不开 PR：{e}")
        write_report(lines)
        return 0

    picks = [p for p in (choice.get("picks") or []) if p.get("sha")]
    if not picks:
        say(lines, "没有值得吸收的候选。")
        write_report(lines)
        return 0
    known = {c[0] for c in cands}
    say(lines, "模型排序：" + " · ".join(
        f"{p['sha']}({p.get('confidence')})" for p in picks))

    # 4) 逐条尝试：挑错一条不该让整天白跑（实测第一次真跑就挑了本仓早已实现过的那条，
    #    冲突后直接收场）。取第一条禁区干净、cherry-pick 无冲突、测试通过的实施。
    chosen = None
    for pick in picks[:3]:
        sha = pick["sha"].strip()
        if sha not in known or int(pick.get("confidence") or 0) < 2:
            say(lines, f"{sha}: 不在候选里或置信不足，跳过。")
            continue
        files = run("git", "show", "--name-only", "--format=", sha).split()
        hits = [f for f in files for bad in FORBIDDEN if f.startswith(bad)]
        if hits:
            say(lines, f"{sha}: 改了禁区文件 {sorted(set(hits))}，跳过。")
            continue
        if args.dry_run:
            say(lines, f"{sha}: dry-run 命中（{pick.get('why', '')}），不 cherry-pick。")
            write_report(lines)
            return 0
        branch = pol["branch_prefix"] + sha
        run("git", "checkout", "-q", "-B", branch, pol["base"])
        code, out = run("git", "cherry-pick", "-x", sha, check=False)
        if code != 0:
            run("git", "cherry-pick", "--abort", check=False)
            say(lines, f"{sha}: cherry-pick 冲突，换下一条。\n```\n" + out[:600] + "\n```")
            continue
        code, out = run("go", "test", "./...", check=False)
        if code != 0:
            say(lines, f"{sha}: 测试未通过，换下一条。\n```\n" + out[-1200:] + "\n```")
            run("git", "checkout", "-q", pol["base"])
            continue
        chosen = (sha, pick, branch)
        break
    if not chosen:
        say(lines, "候选都不满足条件，本次不开 PR（只出报告）。")
        write_report(lines)
        return 0
    sha, pick, branch = chosen
    choice = pick
    say(lines, f"选定 {sha}：{pick.get('why', '')} · 风险 {pick.get('risk', '')}")
    run("git", "push", "-q", "-u", "origin", branch)
    title = f"autobot: 吸收上游 {sha} — {choice.get('why', '')}"[:120]
    body = (f"自动挑选的上游提交 `{sha}`（cherry-pick -x）。\n\n"
            f"- 理由：{choice.get('why', '')}\n- 风险：{choice.get('risk', '')}\n"
            f"- 来源：{up['repo']}@{sha}\n\n"
            f"`go test ./...` 已通过。合并请人工确认。\n")
    run("gh", "label", "create", pol["label"], "--repo", os.environ["GITHUB_REPOSITORY"],
        "--color", "ededed", "--description", "AutoBot 自动挑选的上游改动", check=False)
    run("gh", "pr", "create", "--repo", os.environ["GITHUB_REPOSITORY"], "--base", pol["base"],
        "--head", branch, "--title", title, "--body", body, "--label", pol["label"])
    say(lines, f"已开 PR：{branch}")
    write_report(lines)
    return 0


def write_report(lines):
    with open(REPORT, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    sys.exit(main())
