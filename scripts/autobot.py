#!/usr/bin/env python3
"""AutoBot：把参考仓库的实现读成「本仓可以借鉴什么」，写成 PR 供人审。

两个阶段，可分开跑也可一次跑完：
  analyze    每个参考仓库 clone 到 /tmp/up/<slug>，agent 读代码写差异报告
  implement  按报告自评的值得度排序，取前 N 条让 agent 写 Go 实现并开 PR

硬约束：只推 autobot/* 分支，绝不 push main；提交身份是 GitHub Actions bot。
"""
import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import time

import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPORT = os.path.join(ROOT, "autobot-report.md")
REPORT_DIR = "/tmp/autobot-reports"
UP_DIR = "/tmp/up"
FORBIDDEN = {".github/workflows/", "AGENTS.md", "config.example.json"}
# 只在自产提交上用 -c 传身份；写 git config 会污染本机手写提交（真出过事故）
BOT = ("-c", "user.name=github-actions[bot]",
       "-c", "user.email=41898282+github-actions[bot]@users.noreply.github.com")


def run(*args, check=True, timeout=None):
    p = subprocess.run(args, cwd=ROOT, capture_output=True, text=True, timeout=timeout)
    if check and p.returncode != 0:
        raise RuntimeError(f"{' '.join(args)} 失败: {p.stderr.strip()[:400]}")
    return p.stdout.strip() if check else (p.returncode, (p.stdout + p.stderr).strip())


def say(lines, text=""):
    lines.append(text)
    print(text, flush=True)


def pi(task, timeout):
    """跑一次 PI：cwd=本仓，默认系统提示词与上下文，不做裁剪。

    429 是免费档的常态（并发一高就打满），退避重试三次再认输。
    """
    tail = ""
    for attempt in range(3):
        try:
            code, out = run("pi", "-p", "--provider", os.environ.get("PI_PROVIDER") or "ci",
                            "--model", os.environ["LLM_MODEL"], "--no-session", "--approve",
                            task, check=False, timeout=timeout)
        except subprocess.TimeoutExpired as e:
            # 超时也要留下 agent 最后说的话，否则完全看不出卡在哪一步
            keep = ((e.stdout or "") + (e.stderr or "")).strip().splitlines()[-2:]
            return 124, ("超时｜" + " / ".join(keep))[:300]
        tail = " / ".join((out.strip().splitlines() or [""])[-2:])[:300]
        if code == 0 or "429" not in tail:
            return code, tail
        wait = 60 * (attempt + 1)
        print(f"  429 限流，{wait}s 后重试（{attempt + 1}/3）", flush=True)
        time.sleep(wait)
    return code, tail


def up_clone(src):
    """上游仓完整 clone 到 /tmp/up/<slug>；已存在就浅拉一次更新。"""
    d = os.path.join(UP_DIR, src.replace("/", "__"))
    if os.path.isdir(os.path.join(d, ".git")):
        run("git", "-C", d, "fetch", "-q", "--depth", "200", "origin", check=False)
        return d
    os.makedirs(UP_DIR, exist_ok=True)
    code, _ = run("git", "clone", "-q", "--depth", "200",
                  f"https://github.com/{src}.git", d, check=False)
    return d if code == 0 else ""


def report_path(src):
    return os.path.join(REPORT_DIR, src.replace("/", "__") + ".md")


def report_score(text):
    """报告自评的值得度；没写就按 0（宁可不落地）。"""
    m = re.search(r"值得度\s*[:：]\s*([0-3])", text)
    return int(m.group(1)) if m else 0


def analyze(src, lines):
    """clone → agent 读代码 → 差异报告。返回报告路径（失败为 ""）。"""
    up = up_clone(src)
    if not up:
        say(lines, f"{src}: clone 失败")
        return ""
    os.makedirs(REPORT_DIR, exist_ok=True)
    rp = report_path(src)
    task = (
        f"上游仓库 {src} 已完整 clone 在 `{up}`（只读参考）。本仓是 WorkBuddy 的 Go 网关（当前目录）。\n"
        "任务：分析这个上游仓库与本仓的实现差异——它有哪些本仓还没有的功能、修过的 bug、"
        "更好的实现，哪些值得借鉴，以及要怎么适配到本仓。\n"
        "只读：不要修改本仓任何文件、不要 commit、不要 push。\n"
        f"把结论写进 `{rp}`，严格用这个格式：\n"
        "值得度: 0-3（0=没有可借鉴；3=本仓明确缺这个能力）\n"
        "功能: 上游有什么，解决什么问题\n"
        "差异: 本仓现状（文件:函数）对比上游\n"
        "适配: 要改本仓哪个 文件:函数，怎么改（能给 Go 代码草图更好）\n"
        "优缺点: 这个改法的好处与代价\n"
        "影响: 对现有行为/配置/性能的影响与风险\n"
    )
    code, out = pi(task, int(os.environ.get("PI_ANALYZE_TIMEOUT") or 1500))
    if not os.path.isfile(rp):
        say(lines, f"{src}: agent 未产出报告（退出码 {code}）｜{out}")
        return ""
    say(lines, f"{src}: 分析完成 值得度={report_score(open(rp, encoding='utf-8').read())}")
    return rp


def implement(src, idea, lines):
    """按报告里的适配方案在本仓写 Go 实现。返回 (是否成功, PR 正文材料)。"""
    up = up_clone(src)
    task = (
        (f"上游仓库 {src} 已完整 clone 在 `{up}`（只读参考）。\n" if up else "")
        + "本仓是 Go 项目（当前目录）。上一轮分析的结论：\n"
        f"---\n{idea}\n---\n\n"
        "在本仓用 Go 实现同样的行为，接进真实的调用路径。\n"
        "要求：至少修改一个既有 .go 文件；自己跑 `go build ./...`、`go vet ./...`、"
        "`go test ./...` 全绿；补一条能钉住该行为的测试。\n"
        "不要 git commit、不要 push；不要改 .github/workflows、AGENTS.md、config.example.json。\n"
        "完成后在本仓根目录写 autobot-pr.md，只含这五行：\n"
        "问题: 上游解决什么问题，本仓有没有同类问题\n"
        "改动: 你做了什么\n"
        "位置: 本仓改到的 文件:函数\n"
        "优缺点: 好处与代价\n"
        "影响: 对现有行为/配置/性能的影响与风险\n"
    )
    code, out = pi(task, int(os.environ.get("PI_IMPLEMENT_TIMEOUT") or 2400))
    changed = run("git", "status", "--porcelain", "-uall", check=False)[1].strip()
    if code != 0 or not changed:
        say(lines, f"{src}: 实现未产出（退出码 {code}）｜{out}")
        return False, ""
    return True, read_summary()


def read_summary():
    """agent 写的 PR 正文材料；读完即删，别让它混进提交。"""
    f = os.path.join(ROOT, "autobot-pr.md")
    if not os.path.isfile(f):
        return ""
    text = open(f, encoding="utf-8").read().strip()
    os.remove(f)
    return text


def finish(src):
    """落地闸：外星文件、孤儿实现、build/vet/test 全绿才提交。返回 (ok, 说明)。"""
    status = run("git", "status", "--porcelain", "-uall", check=False)[1].strip()
    if not status:
        return False, "没有改动"
    tracked = set(run("git", "ls-tree", "-r", "--name-only", "HEAD").splitlines())
    changed = [l[3:] for l in status.splitlines()]
    alien = [f for f in changed if f not in tracked and not f.endswith((".go", ".md"))]
    if alien:
        return False, f"引入非 Go 文件 {alien[:5]}"
    # 只新增文件、没接线到既有代码 = 孤儿实现：能编译过测试，但行为没变
    if all(f not in tracked for f in changed):
        return False, f"只新增 {changed[:5]}、未接线到既有文件"
    for cmd in (["go", "build", "./..."], ["go", "vet", "./..."], ["go", "test", "./..."]):
        code, out = run(*cmd, check=False)
        if code != 0:
            return False, f"{' '.join(cmd[1:])} 未通过：{out[-600:]}"
    run("git", "add", "-A")
    run("git", *BOT, "commit", "-q", "-m", f"autobot: 借鉴 {src} 的实现（agent 改写，非原样移植）")
    return True, ""


def land(src, rp, pol, args, lines):
    idea = open(rp, encoding="utf-8").read()
    slug = re.sub(r"[^a-z0-9-]+", "-", src.lower().replace("/", "-")).strip("-")
    branch = f"{pol['branch_prefix']}{slug}-{hashlib.sha1(idea.encode()).hexdigest()[:7]}"
    # 上一条失败可能留下冲突 index，checkout -B 会报「resolve your index first」
    run("git", "reset", "-q", "--hard", "HEAD", check=False)
    run("git", "clean", "-qfd", check=False)
    run("git", "checkout", "-q", "-B", branch, pol["base"])

    ok, summary = implement(src, idea, lines)
    if not ok:
        run("git", "checkout", "-q", pol["base"])
        return 0
    ok, why = finish(src)
    if not ok:
        say(lines, f"{src}: 闸门拦下 —— {why}")
        run("git", "reset", "-q", "--hard", "HEAD")
        run("git", "clean", "-qfd", check=False)
        run("git", "checkout", "-q", pol["base"])
        return 0
    if args.dry_run:
        say(lines, f"{src}: dry-run，不推送（本地分支 {branch}）")
        run("git", "checkout", "-q", pol["base"])
        return 0

    # main 是生产环境：自动化只许推 autobot/* 分支
    assert branch.startswith("autobot/"), branch
    run("git", "push", "-q", "-u", "origin", branch)
    run("gh", "label", "create", pol["label"], "--repo", os.environ["GITHUB_REPOSITORY"],
        "--color", "ededed", "--description", "AutoBot 分析后提出的借鉴改动", check=False)
    body = (summary or f"分析报告：`{os.path.basename(rp)}`\n") + \
        "\n- `go build` / `go vet` / `go test ./...` 已通过；合并前请人工确认行为等价。\n"
    run("gh", "pr", "create", "--repo", os.environ["GITHUB_REPOSITORY"], "--base", pol["base"],
        "--head", branch, "--title", f"autobot: 借鉴 {src} 的实现"[:120],
        "--body", body, "--label", pol["label"])
    say(lines, f"已开 PR：{branch}")
    run("git", "checkout", "-q", pol["base"])
    return 1


def shard(seq, spec):
    """--shard i/n：按直线切片（matrix 并行用）。"""
    if not spec:
        return seq
    i, n = (int(x) for x in spec.split("/"))
    return [x for k, x in enumerate(seq) if k % n == i]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--phase", choices=["analyze", "implement", "all"], default="all")
    ap.add_argument("--shard", default="", help="i/n，只处理该片（matrix 并行用）")
    ap.add_argument("--repo", help="只处理这一个仓库")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    cfg = yaml.safe_load(open(os.path.join(ROOT, "AutoBot.yml"), encoding="utf-8"))
    up, pol = cfg["upstream"], cfg["policy"]
    repos = [args.repo] if args.repo else [r["repo"] if isinstance(r, dict) else r
                                          for r in up["repos"]]
    lines = ["# AutoBot 报告", ""]

    if args.phase in ("analyze", "all"):
        todo = shard(repos, args.shard)
        say(lines, f"分析 {len(todo)} 个仓库（共 {len(repos)}，分片 {args.shard or '全部'}）")
        made = 0
        for src in todo:
            made += 1 if analyze(src, lines) else 0
        if todo and not made:
            # 一个报告都没产出（典型原因：模型额度 429）必须让 job 红，别假装成功
            say(lines, "本片没有产出任何报告")
            write_report(lines)
            return 1

    if args.phase in ("implement", "all"):
        reports = []
        if os.path.isdir(REPORT_DIR):
            for f in os.listdir(REPORT_DIR):
                p = os.path.join(REPORT_DIR, f)
                reports.append((report_score(open(p, encoding="utf-8").read()), p))
        reports.sort(reverse=True)
        picked = [(s, p) for s, p in reports if s >= 2][:int(up.get("land_max") or 2)]
        say(lines, "落地候选：" + (", ".join(f"{os.path.basename(p)}({s})" for s, p in picked) or "无"))
        for _score, p in picked:
            land(os.path.basename(p)[:-3].replace("__", "/"), p, pol, args, lines)

    write_report(lines)
    return 0


def write_report(lines):
    with open(REPORT, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    sys.exit(main())
