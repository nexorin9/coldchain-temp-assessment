#!/usr/bin/env python3
"""把真实捕获的终端会话回显渲染成 PNG（docs/screenshots/ 下的实测证据）。

这个脚本只做一件事：把**真实跑出来的**终端输出排成一张图片。
它不生成任何示意内容——图片里的每一个字都来自捕获文件，
捕获文件由 scripts/capture_evidence.sh 现场跑出来。

用法：
    python3 scripts/render_terminal_shot.py <捕获文件.txt> <输出.png> [--title "标题"]

依赖：Pillow（渲染）、macOS 自带 Menlo 字体（字形）。
"""
from __future__ import annotations

import argparse
import os
import re
import sys
import unicodedata

from PIL import Image, ImageDraw, ImageFont

# 终端配色：底色接近 macOS Terminal 的深色主题，正文用系统等宽字体。
BG = (24, 24, 32)
CHROME = (44, 44, 56)
FG = (222, 224, 230)
TITLE_FG = (168, 170, 180)
PROMPT_FG = (126, 214, 160)
COMMENT_FG = (120, 126, 140)

# 常规 ANSI 前景色（16 色）与其亮色变体。
ANSI = {
    30: (74, 78, 92), 31: (233, 106, 106), 32: (126, 214, 160), 33: (232, 193, 106),
    34: (110, 160, 240), 35: (200, 140, 230), 36: (110, 200, 210), 37: (222, 224, 230),
    90: (120, 126, 140), 91: (245, 140, 140), 92: (150, 226, 180), 93: (240, 210, 140),
    94: (140, 180, 250), 95: (214, 164, 240), 96: (140, 214, 220), 97: (245, 246, 250),
}

FONT_CANDIDATES = [
    "/System/Library/Fonts/Menlo.ttc",
    "/System/Library/Fonts/SFNSMono.ttf",
    "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
]

# 中文字形单独取一款无衬线字体：Menlo 没有汉字字形，
# 直接用它渲染会得到一排豆腐块。这里按字符逐个选字体——
# 拉丁与数字走等宽字体，中文走中文字体，两者基线对齐。
CJK_FONT_CANDIDATES = [
    "/System/Library/Fonts/Hiragino Sans GB.ttc",
    "/System/Library/Fonts/STHeiti Light.ttc",
    "/System/Library/Fonts/Supplemental/Songti.ttc",
    "/Library/Fonts/Arial Unicode.ttf",
]

SGR_RE = re.compile(r"\x1b\[([0-9;]*)m")
OTHER_ESC_RE = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]")


def find_font(size: int) -> ImageFont.FreeTypeFont:
    for path in FONT_CANDIDATES:
        if os.path.exists(path):
            try:
                return ImageFont.truetype(path, size)
            except Exception:
                continue
    return ImageFont.load_default()


def find_cjk_font(size: int):
    for path in CJK_FONT_CANDIDATES:
        if not os.path.exists(path):
            continue
        for index in (0, 1, 2):
            try:
                return ImageFont.truetype(path, size, index=index)
            except Exception:
                continue
    return None


def is_cjk(ch: str) -> bool:
    """判断这个字符是不是要交给中文字体渲染。

    判据比「大于 0x2E80」宽一圈：带圈序号（①②）、全角标点、
    温度单位（℃）这些在等宽字体里往往缺字形，中文字体里有。
    """
    code = ord(ch)
    if code < 0x0080:
        return False
    if code > 0x2E80:
        return True
    if 0x2460 <= code <= 0x24FF:      # 带圈数字与序号
        return True
    if 0x3000 <= code <= 0x303F:      # 中日韩标点
        return True
    if 0xFF00 <= code <= 0xFFEF:      # 全角字符
        return True
    if code in (0x2103, 0x00B0, 0x00D7, 0x2248):
        return True
    return unicodedata.east_asian_width(ch) in ("W", "F", "A")


def parse_ansi(line: str):
    """把一行拆成 [(文本, 颜色, 粗体), ...]。"""
    out = []
    color = FG
    bold = False
    pos = 0
    for match in SGR_RE.finditer(line):
        text = line[pos:match.start()]
        if text:
            out.append((text, color, bold))
        codes = [int(c) for c in match.group(1).split(";") if c != ""] or [0]
        for code in codes:
            if code == 0:
                color, bold = FG, False
            elif code == 1:
                bold = True
            elif code in ANSI:
                color = ANSI[code]
        pos = match.end()
    tail = line[pos:]
    if tail:
        out.append((tail, color, bold))
    return out


def wrap_segments(segments, width):
    """按显示宽度折行（中文按 2 个字符宽计）。"""
    lines = []
    current = []
    used = 0
    for text, color, bold in segments:
        for ch in text:
            w = 2 if ord(ch) > 0x2E80 else 1
            if used + w > width and current:
                lines.append(current)
                current = []
                used = 0
            current.append((ch, color, bold))
            used += w
    lines.append(current)
    return lines


def display_width(text: str) -> int:
    return sum(2 if ord(ch) > 0x2E80 else 1 for ch in text)


def render(src: str, dst: str, title: str, font_size: int = 15) -> None:
    with open(src, encoding="utf-8") as fh:
        raw = fh.read()
    raw = raw.replace("\r\n", "\n").replace("\t", "    ")
    raw = OTHER_ESC_RE.sub(lambda m: m.group(0) if SGR_RE.fullmatch(m.group(0)) else "", raw)

    font = find_font(font_size)
    cjk_font = find_cjk_font(font_size)

    probe = Image.new("RGB", (10, 10))
    draw = ImageDraw.Draw(probe)
    space_w = draw.textlength("M", font=font)

    def char_width(ch: str) -> float:
        if cjk_font is not None and is_cjk(ch):
            return draw.textlength(ch, font=cjk_font)
        return space_w

    def draw_mixed(x: float, y: float, text: str, color, latin_font) -> float:
        for ch in text:
            use = latin_font
            if cjk_font is not None and is_cjk(ch):
                use = cjk_font
            draw.text((x, y), ch, font=use, fill=color, anchor="la")
            x += char_width(ch)
        return x

    max_cols = 0
    rendered_lines = []
    for raw_line in raw.split("\n"):
        segments = parse_ansi(raw_line)
        for seg in wrap_segments(segments, 150):
            rendered_lines.append(seg)
            max_cols = max(max_cols, sum(display_width(t) for t, _, _ in seg))

    pad = 18
    chrome_h = 34
    line_h = int(font_size * 1.55)
    width = int(max(max_cols * space_w + pad * 2, 720))
    height = chrome_h + pad * 2 + line_h * max(len(rendered_lines), 1)

    img = Image.new("RGB", (width, height), BG)
    draw = ImageDraw.Draw(img)

    # 窗口标题条
    draw.rectangle([0, 0, width, chrome_h], fill=CHROME)
    for idx, dot in enumerate([(255, 95, 86), (255, 189, 46), (39, 201, 63)]):
        cx = 18 + idx * 18
        draw.ellipse([cx - 6, chrome_h // 2 - 6, cx + 6, chrome_h // 2 + 6], fill=dot)
    title_font = find_font(max(font_size - 1, 11))
    title_cjk = find_cjk_font(max(font_size - 1, 11))
    tx = 84.0
    for ch in title:
        use = title_cjk if (title_cjk is not None and is_cjk(ch)) else title_font
        draw.text((tx, chrome_h // 2), ch, font=use, fill=TITLE_FG, anchor="lm")
        tx += (draw.textlength(ch, font=title_cjk) if use is title_cjk else space_w)

    y = chrome_h + pad
    for seg in rendered_lines:
        line_text = "".join(t for t, _, _ in seg)
        stripped = line_text.strip()
        if stripped.startswith("$ "):
            draw_mixed(pad, y, line_text, PROMPT_FG, font)
        elif stripped.startswith("#"):
            draw_mixed(pad, y, line_text, COMMENT_FG, font)
        else:
            x = float(pad)
            for text, color, bold in seg:
                x = draw_mixed(x, y, text, color, font)
        y += line_h

    img.save(dst)
    print(f"rendered {dst} ({width}x{height}, {len(rendered_lines)} lines)")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("src")
    parser.add_argument("dst")
    parser.add_argument("--title", default="")
    args = parser.parse_args()
    title = args.title or os.path.basename(args.src)
    render(args.src, args.dst, title)
    return 0


if __name__ == "__main__":
    sys.exit(main())
