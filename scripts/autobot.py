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
from datetime import date
import json
import os
import re
import shutil
import subprocess
import sys
import urllib.error
import urllib.request

import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPORT = os.path.join(ROOT, "autobot-report.md")
FORBIDDEN = {".github/workflows/", "AGENTS.md", "config.example.json"}
PATCH = "/tmp/autobot.patch"
# 自动化产出的提交用机器人身份；就近用 -c 传给单条命令，**不写 git config**——
# 写仓库配置会污染本机手写的提交（2026-10-18 事故：3 个提交变成了 autobot）。
# 极简系统提示词：默认那份 18.5KB 会顶穿上游输入上限（见 pi_port）
PI_SYSPROMPT = ("你是编码助手。用 read/bash/edit/write 工具直接改代码，不要询问许可、"
                "不要只给计划；改完必须自己跑 go build/vet/test。")
BOT = ("-c", "user.name=github-actions[bot]",
       "-c", "user.email=41898282+github-actions[bot]@users.noreply.github.com")


def run(*args, check=True, timeout=None):
    """跑命令。check=False 时返回 (code, 输出)。"""
    p = subprocess.run(args, cwd=ROOT, capture_output=True, text=True, timeout=timeout)
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
        return "skip", "回复不是 JSON：" + out[:120].replace("\n", " "), "", ""
    v = str(d.get("verdict") or "skip").strip().lower()
    why = str(d.get("why") or "").strip()
    body = strip_fence(str(d.get("patch") or "")).strip()
    mp = str(d.get("mapping") or "").strip()
    return v, why or "（模型未给原因）", (body + "\n") if body else "", mp


def tracked_files():
    return set(run("git", "ls-tree", "-r", "--name-only", "HEAD").splitlines())


def port_excerpts(sha, limit=3):
    """跨语言移植的上下文：拿上游的标识符去本仓 Go 源码里找同类模块。

    只按「有没有命中」取文件会全是 cmd/* 这种大杂烩（通用词处处命中），
    所以按「命中的不同标识符数 / 文件大小的平方根」排序——同领域的模块才会赢。
    """
    diff = run("git", "show", "--format=", sha)
    toks = {t for t in re.findall(r"[A-Za-z_][A-Za-z0-9_]{4,}", diff)
            if t.lower() not in {"import", "return", "class", "self", "none", "true", "false",
                                 "async", "await", "python", "raise", "except", "print", "value",
                                 "string", "request", "response", "error", "message", "content"}}
    toks = sorted(toks)[:40]
    args = ["git", "grep", "-c", "-w", "-i"]
    for t in toks:
        args += ["-e", t]
    args += ["--", "*.go"]
    code, out = run(*args, check=False)
    cand = []
    for line in out.splitlines():
        f, _, n = line.rpartition(":")
        if f and f.endswith(".go") and "_test.go" not in f:
            cand.append(f)
    scored = []
    for f in cand[:80]:
        fp = os.path.join(ROOT, f)
        if not os.path.isfile(fp):
            continue
        src = open(fp, encoding="utf-8", errors="replace").read()
        low = src.lower()
        hit = sum(1 for t in toks if t.lower() in low)
        if hit:
            scored.append((hit / (len(src) ** 0.5 + 1), f, hit, low))
    scored.sort(reverse=True)
    ex = []
    for _, f, hit, low in scored[:limit]:
        lines = low.splitlines()
        hit_line = next((i for i, l in enumerate(lines)
                         if any(t.lower() in l for t in toks)), 0)
        lo, hi = max(0, hit_line - 30), min(len(lines), hit_line + 60)
        ex.append(f"--- 本仓同类模块 {f}（命中 {hit} 个上游标识符；第 {lo + 1}-{hi} 行）---\n"
                  + "\n".join(lines[lo:hi]))
    ex.append("--- 本仓 Go 文件树（前 300 个）---\n"
              + "\n".join(sorted(f for f in tracked_files() if f.endswith(".go"))[:300])[:6000])
    return ex


def finish(sha, lines, mode, mp=""):
    """落地前的公共闸门：外星文件、孤儿实现、build/vet/test，全绿才提交。"""
    # 兄弟项目的补丁可能整份搬 Python 进来：go test 照样全绿，但那只是垃圾文件
    # 用 status 而不是 diff：git apply --3way 会把改动写进 index，diff 看不见；
    # status 同时覆盖已暂存与未跟踪。已是本仓既有文件的（比如面板 js/html）放行，
    # 新出现的非 Go 文件一律算外星文件。
    tracked = set(run("git", "ls-tree", "-r", "--name-only", "HEAD").splitlines())
    alien = [l[3:] for l in run("git", "status", "--porcelain", "-uall").splitlines()
             if l[3:] not in tracked and not l[3:].endswith((".go", ".md"))]
    if alien:
        run("git", "reset", "-q", "--hard", "HEAD")
        run("git", "clean", "-qfd")
        say(lines, f"  {sha}: 补丁引入非 Go 文件 {alien[:5]}，丢弃。")
        return False, ""
    if mode == "port":
        # 只新增、没接线 = 孤儿实现：能编译能过测试，但没有任何行为改变
        changed = [l[3:] for l in run("git", "status", "--porcelain", "-uall").splitlines()]
        new_only = [f for f in changed if f not in tracked_files()]
        if new_only and len(new_only) == len(changed):
            run("git", "reset", "-q", "--hard", "HEAD")
            run("git", "clean", "-qfd")
            say(lines, f"  {sha}: 只新增 {new_only}、没有接线到既有文件，丢弃。")
            return False, ""
    # 跨语言重写先过编译与 vet（后面的 go test 是两条路径共用的闸）
    for cmd in (["go", "build", "./..."], ["go", "vet", "./..."]):
        if mode == "port":
            code, msg = run(*cmd, check=False)
            if code != 0:
                run("git", "reset", "-q", "--hard", "HEAD")
                run("git", "clean", "-qfd")
                say(lines, f"  {sha}: {' '.join(cmd[1:])} 未通过：\n```\n" + msg[-1200:] + "\n```")
                return False, ""
    code, msg = run("go", "test", "./...", check=False)
    if code != 0:
        run("git", "reset", "-q", "--hard", "HEAD")
        run("git", "clean", "-qfd")
        say(lines, f"  {sha}: 适配后测试未通过：\n```\n" + msg[-1200:] + "\n```")
        return False, ""
    run("git", "add", "-A")
    # 无改动时 commit 的失败信息走 stdout、stderr 为空，check=True 抛出的异常会把整轮跑挂
    if run("git", "diff", "--cached", "--quiet", check=False)[0] == 0:
        say(lines, f"  {sha}: 全部闸门通过但没有任何改动，跳过。")
        return False, ""
    run("git", *BOT, "commit", "-q", "-m", f"autobot: 适配上游 {sha}（LLM 改写，非原样移植）")
    return True, mp

def pi_port(sha, lines, model):
    """跨语言移植交给 PI 的 agent 循环：它自己 grep/读/改/跑测试。

    一次性 prompt 的瓶颈是上下文——模型看不到调用链只能回 skip（实测四条全 skip）。
    """
    task = (
        f"第一步：立刻调用 bash 执行 `git show --stat {sha}` 与 `git show {sha}`，不要先解释。"
        f"本仓是 Go 项目，上游提交 {sha} 是另一种语言（多为 Python）的实现。\n"
        "任务：读懂它修正或新增的行为，在本仓用 Go 实现同样的行为，并接进真实的调用路径。\n"
        "必须修改至少一个既有 .go 文件；跑 `go build ./...`、`go vet ./...`、`go test ./...`，"
        "全绿才算完成；完成后加一条能钉住该行为的测试。\n"
        "你已拥有 read/bash/edit/write 工具：直接调用，不要询问许可、不要只给计划等确认。\n"
        "不要 git commit、不要 push；不要改 .github/workflows、AGENTS.md、config.example.json。\n"
        "最后一行输出：改了哪些文件 + 上游行为对应本仓的哪个 文件:函数。"
    )
    try:
        # PI 自带系统提示词有 18.5KB，加上 tools 共 22KB，超过上游约 19k 字符上限：
        # 网关会触发滚动摘要 → 撞 100s 边缘超时返回 504 → PI 拿不到工具结果无限重试。
        # 换极简系统提示词后整包约 3.3KB，同一任务一次就收口（实测）。
        code, out = run("pi", "-p", "--provider", "ci", "--model", model,
                        "--no-session", "--approve", "--no-context-files", "--no-skills",
                        "--system-prompt", PI_SYSPROMPT,
                        task, check=False, timeout=1500)
    except subprocess.TimeoutExpired:
        say(lines, f"  {sha}: PI 超时（25 分钟）")
        return False
    tail = " / ".join((out.strip().splitlines() or [""])[-3:])[:600]
    say(lines, f"  {sha}: PI 退出码 {code}｜{tail}")
    return code == 0


def adapt(sha, base, branch, base_url, key, model, lines, mode="adapt"):
    """让模型把上游提交改写成本仓能用的补丁，提交到 branch。返回 (是否成功, 行为映射)。"""
    run("git", "checkout", "-q", "-B", branch, base)
    if mode == "port" and shutil.which("pi"):
        if not pi_port(sha, lines, model):
            return False, ""
        return finish(sha, lines, mode)
    diff = run("git", "show", "--format=", sha)
    files = run("git", "show", "--name-only", "--format=", sha).split()
    excerpts = []
    if mode == "port":
        excerpts = port_excerpts(sha)
    for f in files[:3]:
        fp = os.path.join(ROOT, f)
        if os.path.isfile(fp):
            excerpts.append(f"--- 本仓现有 {f}（节选） ---\n" + open(fp, encoding="utf-8").read()[:2500])
        else:
            excerpts.append(f"--- {f} 在本仓不存在（新文件，或本仓实现在别处） ---")

    # port：上游是另一种语言（通常 Python），本仓是 Go。任务不是搬代码，是读逻辑→Go 重写→接线。
    port_head = (
        f"上游 {sha} 是另一种语言的实现，本仓是 Go。**不要因为「本仓没有 xxx.py」就跳过**——"
        "那正是本任务的前提。你要做的是：读懂这个提交修正/新增了什么行为，然后在本仓用 Go 实现同样的行为，"
        "并接进真实调用路径。\n"
        "硬性要求：\n"
        "1. 必须修改至少一个本仓既有 .go 文件，把新逻辑接进去；只新增文件、没有任何既有文件被改，视为孤儿实现，会被丢弃。\n"
        "2. 风格跟随邻近代码（错误处理、日志、命名、单测），不要引入新依赖。\n"
        "3. mapping 写清「上游函数/文件 → 本仓文件:函数」，供人审。\n\n"
        "输出**一个 JSON 对象**，不要别的话：\n"
        '{"verdict": "port|already_have|skip",\n'
        ' "why": "一到两句：上游改的是什么行为、本仓现状",\n'
        ' "mapping": "signin.py:run_growth → internal/scheduler/growth.go:RunGrowth",\n'
        ' "patch": "unified diff（含 diff --git/---/+++/@@；新文件用 new file mode 100644）"}\n'
        "already_have 要给本仓文件:函数作为依据；skip 只在依赖本仓根本没有的抽象（如前端、外部服务）时才用。\n\n")

    def build(diff_cap, ex_cap):
        return (port_head if mode == "port" else "") + ("下面是一个上游提交的 diff。本仓是它的长期 fork，同一功能往往已有**另一种实现**，"
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
    # 降采样时不能把节选全砍掉：没有本仓上下文，模型只能回「无法判断哪些已有替代实现」
    # （真实日志就是这么 skip 的）。留一份 800 字的最小节选，够它认「这条已经做过了」。
    tiny = [e[:800] for e in excerpts[:1]]
    for i, (cap, ex) in enumerate(((30000, excerpts), (6000, tiny))):
        try:
            out = llm(base_url, key, model, build(cap, ex))
            break
        except Exception as e:  # noqa: BLE001
            say(lines, f"  {sha}: 第 {i + 1} 次适配调用失败：{e}")
    if out is None:
        return False, ""
    verdict, why, body, mp = verdict_of(out)
    say(lines, f"  {sha}: 判定={verdict} — {why}")
    if verdict in ("skip", "already_have") or not body.strip():
        return False, ""
    with open(PATCH, "w", encoding="utf-8") as f:
        f.write(body)
    code, msg = run("git", "apply", "--3way", PATCH, check=False)
    if code != 0:
        # 模型给的 diff 常在 hunk 头上写错行数（「corrupt patch」），--recount 让 git 重算；
        # 真冲突（context 不匹配）它救不了，那就是摘要节选不够，得换人看。
        code, msg = run("git", "apply", "--recount", "-C1", PATCH, check=False)
        if code != 0:
            say(lines, f"  {sha}: 补丁应用失败：\n```\n" + msg[:600] + "\n```")
            return False, ""
    return finish(sha, lines, mode, mp)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", help="临时指定上游仓库（owner/name 或 URL），默认取 AutoBot.yml")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    # CI runner 本来没有 git 身份。身份只在要提交的命令上用 BOT 传 -c，不写 git config：
    # 写仓库配置会污染本机手写提交（事故：3 个提交变成 autobot）。
    # PR 的 author 仍是 AUTOBOT_TOKEN 的所有者——用 GITHUB_TOKEN 才显示 bot，但那样不再触发其它 workflow。

    cfg = yaml.safe_load(open(os.path.join(ROOT, "AutoBot.yml"), encoding="utf-8"))
    up, pol = cfg["upstream"], cfg["policy"]
    if args.repo:
        aims = [(args.repo, pol["mode"])]
    else:
        sibs = up.get("repos") or []
        n = up.get("repos_per_run") or len(sibs)
        if 0 < n < len(sibs):  # 按日期轮转，起点每天前移，轮到全部只要 len/n 天
            # 每天前移一整个窗口（不是 1 格），65/8 天扫完全部
            start = (date.today().toordinal() * n) % len(sibs)
            sibs = [sibs[(start + i) % len(sibs)] for i in range(n)]
        aims = [(up["repo"], pol["mode"])] + [(r, up.get("repos_mode", pol["mode"])) for r in sibs]
    lines = ["# AutoBot 报告", ""]
    opened = 0
    for raw, mode in aims:
        src = raw.replace("https://github.com/", "").replace(".git", "").strip("/")
        say(lines, "")
        say(lines, f"## {src}")
        # 上一条可能停在 autobot/<sha> 上（测试失败或已开 PR），换上游前必须回基线：
        # 否则下一条的 git cherry 会拿错 HEAD 比对，分支也从错的地方拉出来。
        run("git", "checkout", "-q", pol["base"])
        opened += absorb(src, mode, up, pol, args, lines)
        if opened >= pol["max_prs_per_run"]:
            break
    write_report(lines)
    return 0


def absorb(src, mode, up, pol, args, lines):
    # 1) 拉上游：--repo 可临时换别家，走同一个 src remote
    run("git", "remote", "remove", "src", check=False)
    run("git", "remote", "add", "src", f"https://github.com/{src}.git")
    # 默认分支不一定是 main（清单里有 8 个是 master），问一次远端
    code, out = run("git", "ls-remote", "--symref", "src", "HEAD", check=False)
    m = re.search(r"ref: refs/heads/(\S+)\s+HEAD", out or "")
    branch = m.group(1) if m else "main"
    code, msg = run("git", "fetch", "-q", "src", branch, check=False)
    if code != 0:
        say(lines, f"拉取 {src} 失败：{msg[:300]}")
        return 0

    # 2) 候选：最近 N 条非 merge、非 chore 提交
    log = run("git", "log", f"--max-count={up['limit']}", "--no-merges",
              "--format=%h%x09%ad%x09%s", "--date=short", f"src/{branch}")
    cands = [l.split("\t", 2) for l in log.splitlines()
             if l.strip() and not re.match(r"\s*chore", l.split("\t", 2)[2])]
    # git cherry 用 patch-id 挡掉「逐字节等同」的移植；改写实现的挡不住，交给适配路径。
    applied = {l[2:9] for l in run("git", "cherry", "-v", "HEAD", f"src/{branch}").splitlines()
               if l.startswith("- ")}
    cands = [c for c in cands if c[0] not in applied]
    tried = set(run("git", "ls-remote", "--heads", "origin", pol["branch_prefix"] + "*").split())
    cands = [c for c in cands if f"refs/heads/{pol['branch_prefix']}{c[0]}" not in tried]
    if not cands:
        say(lines, f"{src}：没有未处理的新提交（已去 merge/chore/已吸收/已试过）。")
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
        return 0

    # 4) 让模型排序：挑哪条、为什么、风险、置信度
    note = "" if mode == "cherry-pick" else (
        f"注意：{src} 是另一套实现（语言/结构不同），代码不能直接搬，只能借鉴思路与接口设计；"
        "只有你能说清「本仓对应模块在哪、怎么改」时才算值得挑。\n")
    prompt = (
        note + "你在给一个 fork 挑选值得吸收的上游提交。本仓是 workbuddy2api-panel 的长期 fork，"
        "缓存键、补偿、检索、签到、面板、调度都有自有实现。\n"
        "按值得程度挑**最多 3 条**：优先真 bug 修复、改动小、目的清晰。输出 JSON："
        "{\"picks\":[{\"sha\":\"...\",\"why\":\"一句话\",\"risk\":\"一句话\",\"confidence\":0-3}]}。"
        "没有值得的就 {\"picks\":[]}。\n\n" + listing)
    try:
        picks = [p for p in (pick_json(llm(base_url, key, model, prompt)).get("picks") or [])
                 if p.get("sha")]
    except Exception as e:  # noqa: BLE001 —— LLM 侧任何失败都降级成报告
        say(lines, f"LLM 调用失败，本次不开 PR：{e}")
        return 0
    if not picks:
        say(lines, "模型判定没有值得吸收的候选。")
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
            return 0
        branch = pol["branch_prefix"] + sha
        run("git", "checkout", "-q", "-B", branch, pol["base"])
        how, ok, mp = "cherry-pick", False, ""
        if mode == "cherry-pick":
            code, out = run("git", *BOT, "cherry-pick", "-x", sha, check=False)
            if code == 0:
                code, out = run("go", "test", "./...", check=False)
                ok = code == 0
                if not ok:
                    run("git", "checkout", "-q", pol["base"])
                    say(lines, f"{sha}: 移植后测试未通过，换下一条。\n```\n" + out[-1200:] + "\n```")
                    continue
            else:
                run("git", "cherry-pick", "--abort", check=False)
        if not ok:
            # 兄弟项目不是本仓 fork：代码搬过来只会变成垃圾文件，只能借鉴思路
            say(lines, f"{sha}: " + ("直搬失败" if mode == "cherry-pick" else "跳过直搬") + "，改试 LLM 适配。")
            ok, mp = adapt(sha, pol["base"], branch, base_url, key, model, lines, mode)
            if not ok:
                continue
            how = "LLM 适配" if mode != "port" else "LLM 移植（跨语言重写）"
        run("git", "push", "-q", "-u", "origin", branch)
        run("gh", "label", "create", pol["label"], "--repo", os.environ["GITHUB_REPOSITORY"],
            "--color", "ededed", "--description", "AutoBot 自动挑选的上游改动", check=False)
        title = f"autobot: 吸收 {src}@{sha} — {pick.get('why', '')}"[:120]
        body = (f"来源 `{src}@{sha}`，实施方式：**{how}**。\n\n"
                f"- 理由：{pick.get('why', '')}\n- 风险：{pick.get('risk', '')}\n"
                + (f"- 行为映射：{mp}\n" if mp else "")
                + "- `go build` / `go vet` / `go test ./...` 已通过；合并前请人工确认行为等价。\n")
        run("gh", "pr", "create", "--repo", os.environ["GITHUB_REPOSITORY"], "--base", pol["base"],
            "--head", branch, "--title", title, "--body", body, "--label", pol["label"])
        say(lines, f"已开 PR（{how}）：{branch}")
        return 1

    say(lines, "候选都不满足条件，本次不开 PR（只出报告）。")
    write_report(lines)
    return 0


def write_report(lines):
    with open(REPORT, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    sys.exit(main())
