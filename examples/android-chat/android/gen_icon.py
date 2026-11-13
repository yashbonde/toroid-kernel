#!/usr/bin/env python3
"""Writes Toroid Chat's pixel-art launcher icon and notification icon:

  res/drawable/ic_launcher_fg.xml   adaptive-icon foreground (the chosen ART)
  res/drawable/ic_stat.xml          notification icon: the header orb at rest

The launcher art sits on a 36x36 grid of 3dp cells, which is the 108dp
adaptive-icon layer. The art itself is at most 22x22 cells (66dp, the safe
zone), centred. A launcher shows the 108dp layer at about 2/3 of its size,
so a cell lands on 2dp, the grid of Pixel Bar's font and navigation buttons.
Each colour is one path of whole-cell rects, so edges stay crisp.

Digits 0-4 index the orb's green ramp (web/index.html, ORB_PAL); '.' is
empty and shows the background, @color/icon_bg. Edit the art and rerun:

  python3 examples/android-chat/android/gen_icon.py
"""
import math
import os

PAL = ["#12261D", "#2A5A45", "#4F9A78", "#7CC4A4", "#C9F0DD"]
CELL, GRID = 3, 36  # dp per cell, cells per 108dp layer

ART = {
    # A torus seen face-on, lit from the upper left, with rim sparks like the
    # header orb's while it works. The chosen icon.
    "ring": [
        "......................",
        "......................",
        "........444444........",
        "......4444433333..4...",
        ".....444433333333.....",
        "....44433322333332....",
        "...4443321122233222...",
        "...443321....233222.3.",
        "..444321......333222..",
        "..44331........33221..",
        "..44321........33221..",
        "..43322........43211..",
        "..43332........43211..",
        "..433322......432211..",
        ".3.333333....443211...",
        "...3333333344332111...",
        "....33223333322111....",
        ".....222222222111.....",
        "...4..2222211111......",
        "........211111........",
        "......................",
        "......................",
    ],
    # The torus as a speech bubble, its tail at the lower left.
    "bubble": [
        "......................",
        "......................",
        "......................",
        "........44444433......",
        "......44444333333.....",
        ".....4444333333333....",
        ".....44332222233322...",
        "....4443211112233222..",
        "....44321.....333222..",
        "...443321......33222..",
        "...44321.......43221..",
        "...44322.......43221..",
        "...43332.......43221..",
        "....33322......43211..",
        "....333333...4432211..",
        "....333333344432211...",
        "....333222333322111...",
        "....32222222221111....",
        "....221222221111......",
        "....1.....1111........",
        "......................",
        "......................",
    ],
    # A wider torus with a typing indicator in its hole.
    "dots": [
        "......................",
        ".........4444.........",
        "......4444443333......",
        ".....444433333333.....",
        "....44433322333333....",
        "...4443221122233322...",
        "..4443211....2233222..",
        "..44321........33222..",
        "..44321........33322..",
        ".44331..........33221.",
        ".44321.44.33.22.43221.",
        ".44322.44.33.22.43211.",
        ".43332..........43211.",
        "..33322........43221..",
        "..33332........43211..",
        "..3333333....4432111..",
        "...3333333444332111...",
        "....32223333322111....",
        ".....222222222111.....",
        "......2222211111......",
        ".........1111.........",
        "......................",
    ],
}
CHOSEN = "ring"


def runs(rows, keep):
    """Yields (x, y, width) for each horizontal run of cells where keep(ch)."""
    for y, row in enumerate(rows):
        x = 0
        while x < len(row):
            if keep(row[x]):
                w = 1
                while x + w < len(row) and keep(row[x + w]):
                    w += 1
                yield x, y, w
                x += w
            else:
                x += 1


def rects(rows, keep, ox=0, oy=0):
    return "".join(f"M{ox + x},{oy + y}h{w}v1h-{w}z" for x, y, w in runs(rows, keep))


def check(name, rows):
    n = len(rows)
    assert n <= 22 and all(len(r) == n for r in rows), f"{name}: art must be square, at most 22 cells"
    c = n / 2
    for y, row in enumerate(rows):
        for x, ch in enumerate(row):
            assert ch in ".01234", f"{name}: bad cell {ch!r}"
            if ch != "." and math.hypot(x + 0.5 - c, y + 0.5 - c) > 11:
                raise SystemExit(f"{name}: cell ({x},{y}) is outside the 66dp safe zone")


def orb_at_rest():
    """The header orb at rest (drawOrb in web/index.html): an 11x11 ring."""
    return [
        "".join("#" if 3.4 < math.hypot(x - 5, y - 5) < 5.3 else "." for x in range(11))
        for y in range(11)
    ]


def main():
    res = os.path.join(os.path.dirname(os.path.abspath(__file__)), "res", "drawable")
    for name, rows in ART.items():
        check(name, rows)
    rows = ART[CHOSEN]
    off = (GRID - len(rows)) // 2
    # The darkest colour fills the whole silhouette and the others paint over
    # it, so anti-aliased seams between colours never show the background.
    used = [i for i in range(len(PAL)) if any(str(i) in r for r in rows)]
    paths = "\n".join(
        f'    <path android:fillColor="{PAL[i]}"\n        android:pathData="'
        + rects(rows, (lambda ch: ch != ".") if i == used[0] else (lambda ch, i=i: ch == str(i)), off, off)
        + '" />'
        for i in used
    )
    art = "\n".join("     " + r.replace(".", " ") for r in rows)
    with open(os.path.join(res, "ic_launcher_fg.xml"), "w") as f:
        f.write(f'''<?xml version="1.0" encoding="utf-8"?>
<!-- Toroid Chat's launcher icon ("{CHOSEN}"), pixel art on 3dp cells in the
     108dp adaptive-icon layer, inside the 66dp safe zone. Generated by
     android/gen_icon.py; edit the art there and rerun.
{art}
-->
<vector xmlns:android="http://schemas.android.com/apk/res/android"
    android:width="108dp" android:height="108dp"
    android:viewportWidth="{GRID}" android:viewportHeight="{GRID}">
{paths}
</vector>
''')

    orb = orb_at_rest()
    art = "\n".join("     " + r.replace(".", " ").replace("#", "4") for r in orb)
    with open(os.path.join(res, "ic_stat.xml"), "w") as f:
        f.write(f'''<?xml version="1.0" encoding="utf-8"?>
<!-- Notification icon: the header orb at rest, on 2dp cells. Generated by
     android/gen_icon.py.
{art}
-->
<vector xmlns:android="http://schemas.android.com/apk/res/android"
    android:width="24dp" android:height="24dp"
    android:viewportWidth="12" android:viewportHeight="12">
    <path android:fillColor="#FFFFFFFF"
        android:pathData="{rects(orb, lambda ch: ch == "#", 0.5, 0.5)}" />
</vector>
''')
    print(f"wrote ic_launcher_fg.xml ({CHOSEN}) and ic_stat.xml in {res}")


if __name__ == "__main__":
    main()
