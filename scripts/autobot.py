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
import signal
import subprocess
import sys
import tempfile
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

    shortcut: 用 Popen + 文件重定向而不是 subprocess.run(capture_output)，
    因为 PI 拉起的工具子进程会继承 stdout 管道：kill 掉 pi 之后管道仍开着，
    run() 收尾时永远等下去（实测挂到 job 超时 48 分钟都没退）。
    """
    tail = ""
    for attempt in range(3):
        with tempfile.TemporaryFile("w+") as fh:
            p = subprocess.Popen(
                ["pi", "-p", "--provider", os.environ.get("PI_PROVIDER") or "ci",
                 "--model", os.environ["LLM_MODEL"], "--no-session", "--approve",
                 "--mode", "json", "-t", "read,write", task],
                cwd=ROOT, stdout=fh, stderr=subprocess.STDOUT, text=True,
                start_new_session=True)
            try:
                p.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(p.pid, signal.SIGKILL)   # 连它拉起的工具子进程一起杀
                except ProcessLookupError:
                    pass
                p.wait(30)
            fh.seek(0)
            out = fh.read()
        code = 124 if (p.returncode is None or p.returncode < 0) else p.returncode
        keep = (out.strip().splitlines() or [""])[-3:]
        tail = " / ".join(keep)[:300]
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
        run("git", "-C", d, "fetch", "-q", "--depth", "200", "origin", check=False, timeout=600)
        return d
    os.makedirs(UP_DIR, exist_ok=True)
    code, _ = run("git", "clone", "-q", "--depth", "200",
                  f"https://github.com/{src}.git", d, check=False, timeout=600)
    return d if code == 0 else ""


def upstream_brief(up):
    """上游清单：文件表 + 最近提交 + README 摘要。

    shortcut: 不让 agent 自己在仓里翻。deepseek 走网关每轮往返几十秒，
    让它探索 100+ 文件的仓 25 分钟也跑不完（实测三次全超时）。
    """
    files = run("git", "-C", up, "ls-files", check=False)[1].splitlines()[:200]
    logs = run("git", "-C", up, "log", "--oneline", "-40", "--no-merges", check=False)[1]
    readme = ""
    for n in ("README.md", "readme.md", "README.zh-CN.md", "README_EN.md"):
        f = os.path.join(up, n)
        if os.path.isfile(f):
            readme = open(f, encoding="utf-8", errors="replace").read()[:2500]
            break
    return files, logs, readme


def report_path(src):
    return os.path.join(REPORT_DIR, src.replace("/", "__") + ".md")


def report_score(text):
    """报告自评的值得度；没写就按 0（宁可不落地）。"""
    m = re.search(r"值得度\s*[:：]\s*([0-3])", text)
    return int(m.group(1)) if m else 0


def own_brief():
    """本仓 Go 文件清单：让 agent 判「本仓现状」时有据可依，而不是靠猜。

    shortcut: 只给路径不给内容，读不读由 agent 自己定（预算内最多 2 个）。
    """
    return run("git", "ls-files", "*.go", check=False)[1].splitlines()[:250]


def analyze(src, lines):
    """clone → agent 读代码 → 差异报告。返回报告路径（失败为 ""）。"""
    up = up_clone(src)
    if not up:
        say(lines, f"{src}: clone 失败")
        return ""
    os.makedirs(REPORT_DIR, exist_ok=True)
    rp = report_path(src)
    files, logs, readme = upstream_brief(up)
    task = (
        f"上游仓库 {src} 已 clone 在 `{up}`（只读参考）。本仓是 WorkBuddy 的 Go 网关（当前目录）。\n"
        "任务：分析这个上游仓库与本仓的实现差异——它有哪些本仓还没有的功能、修过的 bug、"
        "更好的实现，哪些值得借鉴，以及要怎么适配到本仓。\n"
        f"上游已有清单，不用自己遍历：\n文件（{len(files)} 个，最多）：\n"
        + "\n".join(files) + "\n最近提交：\n" + logs
        + ("\nREADME 摘要：\n" + readme if readme else "")
        + "\n**最多读 6 个文件**（挑这 6 个里最能说明差异的），20 分钟内必须写出报告。\n"
        "本仓 Go 文件清单（写「差异」时对照）：\n" + "\n".join(own_brief()) + "\n"
        "**「差异」一行必须点名本仓的 文件:函数**。若读了本仓代码确认已有等价实现，"
        "就写「本仓已有：<文件:函数>」并把值得度降到 0-1，不要报 3。\n"
        "（上一轮报 3 的那条，implement 一读代码就发现本仓早就有了，白跑一整轮。）\n"
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


def adapt_brief(idea):
    """从报告里只取「功能」和「适配」两段。

    shortcut: 整篇报告（含 200 行文件表）喂进去后模型会重新理解全仓，
    60 分钟改不完一处（实测超时两次）。
    """
    want = []
    for m in re.finditer(r"^#{2,4}\s*(.+)$", idea, re.M):
        title = m.group(1)
        start = m.end()
        nxt = idea.find("\n#", start)
        body = idea[start:nxt if nxt > 0 else len(idea)]
        if "适配" in title or "功能" in title or "改动" in title:
            want.append(f"{title}\n{body.strip()[:1800]}")
    return "\n\n".join(want)[:4000] or idea[:2500]


def implement(src, idea, lines):
    """两次短调用：先出改动方案，再出 unified diff。

    shortcut: 一次调用干完「读代码→判断→写实现→写测试→接线」时 7 轮全败
    （只写计划/超时/不接线/抄垃圾/判定无需改动），单次失败要 90 分钟；
    拆成 10 分钟 + 20 分钟两段，失败便宜，方案段还能给 diff 段当输入。
    """
    up = up_clone(src)
    brief = adapt_brief(idea)
    where = (f"上游仓库 {src} 已 clone 在 `{up}`（只读参考）。\n" if up else "") + \
        "本仓 Go 文件清单（不要猜路径，就照这个读）：\n" + "\n".join(own_brief()) + "\n"
    plan_f = os.path.join(ROOT, "autobot-plan.md")
    patch = os.path.join(ROOT, "autobot.patch")
    for f in (plan_f, patch):
        if os.path.isfile(f):
            os.remove(f)

    # 第一段：只出方案，不许写其它文件
    code, out = pi(
        where
        + "本仓是 Go 项目（当前目录）。分析结论：\n"
        f"---\n{brief}\n---\n\n"
        "只做一件事：read「适配」里点名的那几个文件，然后把改动方案写进 "
        f"`{plan_f}`——要改哪个 文件:函数、改成什么、测试加在哪，每处给出准确的位置与代码。\n"
        "不要写实现文件、不要写 diff、不要跑 go build。禁止输出计划说明，直接写文件。\n",
        int(os.environ.get("PI_PLAN_TIMEOUT") or 1800))
    if not os.path.isfile(plan_f):
        say(lines, f"{src}: 改动方案未产出（退出码 {code}）｜{out}")
        return False, ""
    plan_txt = open(plan_f, encoding="utf-8").read()[:8000]
    os.remove(plan_f)

    # 第二段：按方案出 diff
    code, out = pi(
        where
        + "本仓是 Go 项目（当前目录）。要落地的改动方案：\n"
        f"---\n{plan_txt}\n---\n\n"
        "把它写成标准 unified diff 存进本仓根目录的 `autobot.patch`，不要动工作区任何文件：\n"
        "--- a/internal/foo.go\n+++ b/internal/foo.go\n@@ -10,6 +10,8 @@\n 上下文行\n-旧行\n+新行\n"
        "**只能修改已存在的文件**；测试加进已存在的 _test.go。禁止输出说明，直接写 patch。\n"
        "不要 git commit、不要 push；不要改 .github/workflows、AGENTS.md、config.example.json。\n"
        f"写完后在同一目录写 autobot-pr.md，只含这五行：\n"
        "问题: 上游解决什么问题，本仓有没有同类问题\n"
        "改动: 你做了什么\n"
        "位置: 本仓改到的 文件:函数\n"
        "优缺点: 好处与代价\n"
        "影响: 对现有行为/配置/性能的影响与风险\n",
        int(os.environ.get("PI_IMPLEMENT_TIMEOUT") or 1800))
    if not os.path.isfile(patch):
        say(lines, f"{src}: 实现未产出（退出码 {code}）｜{out}")
        return False, ""
    ok, why = apply_patch(patch)
    os.remove(patch)
    if not ok:
        say(lines, f"{src}: patch 应用失败｜{why}")
        return False, ""
    if not run("git", "status", "--porcelain", "-uall", check=False)[1].strip():
        say(lines, f"{src}: patch 应用后没有改动")
        return False, ""
    return True, read_summary()


def apply_patch(patch):
    """模型手写的 unified diff 行号常常不准：--recount 重算 hunk 头，-C1 放宽上下文。

    shortcut: 不做 fuzzy 合并，三次都失败就算这条候选没成（人看的报告还在）。
    """
    # 模型常把 diff 包在 ```diff 里或前面带说明，剥掉再喂 git apply
    raw = open(patch, encoding="utf-8").read()
    lines = [l for l in raw.splitlines() if not l.startswith("```")]
    start = next((i for i, l in enumerate(lines) if l.startswith(("--- ", "diff --git"))), 0)
    open(patch, "w", encoding="utf-8").write("\n".join(lines[start:]) + "\n")
    err = ""
    for extra in (["--recount", "-C1"], ["--recount", "-C1", "--3way"], ["--recount", "-C0"]):
        r = subprocess.run(["git", "apply", *extra, patch], cwd=ROOT,
                           capture_output=True, text=True)
        if r.returncode == 0:
            return True, ""
        err = (r.stderr or r.stdout).strip()[:300]
    return False, err


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
    # agent 常把上游的 .bat/.sh 一起抄进来。这些本来就不该进本仓，删掉继续，
    # 不要因为一条垃圾文件把整条实现丢掉（真丢过一整轮）。
    alien = [f for f in changed if f not in tracked and not f.endswith((".go", ".md"))]
    if alien:
        run("git", "reset", "-q", "HEAD", "--", *alien, check=False)
        for f in alien:
            if os.path.isfile(f):
                os.remove(f)
        changed = [f for f in changed if f not in alien]
        if not changed:
            return False, "清掉外星文件后没有改动"
    status = run("git", "status", "--porcelain", "-uall", check=False)[1].strip()
    if not status:
        return False, "清掉外星文件后没有改动"
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
        landed = sum(1 for _s, p in picked
                     if land(os.path.basename(p)[:-3].replace("__", "/"), p, pol, args, lines))
        if picked and not landed and not args.dry_run:
            # 有候选却一条都没落成（超时/闸门）必须红，别报成功
            say(lines, "有候选但没有任何一条落地")
            write_report(lines)
            return 1

    write_report(lines)
    return 0


def write_report(lines):
    with open(REPORT, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    sys.exit(main())
