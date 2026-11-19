#!/usr/bin/env python3
"""Launcher for the forge binary.

The suite runs the entry as `python3 /bot/main.py` at the repository root,
so this script resolves the statically linked `forge` binary relative to its
own location and execs it (stdlib only, no arguments of its own).
"""
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
FORGE = os.path.join(HERE, "forge")

if __name__ == "__main__":
    os.execv(FORGE, [FORGE] + sys.argv[1:])
