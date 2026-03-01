#!/usr/bin/env python3
"""
Analisa um session log JSON do N-Backup e aponta possiveis missing chunks.

Uso:
    check-missing-chunks.py <session_file_path>

Exit codes:
    0 = nenhum problema encontrado
    1 = possiveis chunks faltando / incompletos
    2 = erro de uso, leitura ou parse
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Iterable


def usage() -> int:
    print("Uso: check-missing-chunks.py <session_file_path>", file=sys.stderr)
    return 2


def parse_seq(entry: dict) -> int | None:
    value = entry.get("globalSeq")
    if isinstance(value, int):
        return value
    if isinstance(value, str):
        try:
            return int(value)
        except ValueError:
            return None
    return None


def values_to_ranges(values: Iterable[int]) -> list[tuple[int, int]]:
    ordered = sorted(set(values))
    if not ordered:
        return []

    ranges: list[tuple[int, int]] = []
    start = ordered[0]
    end = ordered[0]
    for value in ordered[1:]:
        if value == end + 1:
            end = value
            continue
        ranges.append((start, end))
        start = value
        end = value
    ranges.append((start, end))
    return ranges


def missing_ranges(values: set[int]) -> list[tuple[int, int]]:
    ordered = sorted(values)
    if len(ordered) < 2:
        return []

    gaps: list[tuple[int, int]] = []
    prev = ordered[0]
    for value in ordered[1:]:
        if value > prev + 1:
            gaps.append((prev + 1, value - 1))
        prev = value
    return gaps


def count_range_items(ranges: Iterable[tuple[int, int]]) -> int:
    return sum(end - start + 1 for start, end in ranges)


def format_ranges(ranges: Iterable[tuple[int, int]], limit: int = 12) -> str:
    range_list = list(ranges)
    rendered = []
    total = 0
    for start, end in range_list:
        total += 1
        rendered.append(str(start) if start == end else f"{start}-{end}")
        if total >= limit:
            break

    if not rendered:
        return "nenhum"

    extra = len(range_list) - len(rendered)
    if extra > 0:
        rendered.append(f"... (+{extra} intervalos)")
    return ", ".join(rendered)


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        return usage()

    log_path = Path(argv[1])
    if not log_path.is_file():
        print(f"Arquivo nao encontrado: {log_path}", file=sys.stderr)
        return 2

    started: set[int] = set()
    received: set[int] = set()
    sacked: set[int] = set()
    failed: set[int] = set()
    explicit_missing: set[int] = set()

    parsed_lines = 0
    invalid_lines = 0
    blank_lines = 0

    try:
        with log_path.open("r", encoding="utf-8") as handle:
            for raw_line in handle:
                line = raw_line.strip()
                if not line:
                    blank_lines += 1
                    continue

                try:
                    entry = json.loads(line)
                except json.JSONDecodeError:
                    invalid_lines += 1
                    continue

                if not isinstance(entry, dict):
                    invalid_lines += 1
                    continue

                parsed_lines += 1
                msg = entry.get("msg")
                seq = parse_seq(entry)

                if msg == "chunk_receive_started" and seq is not None:
                    started.add(seq)
                elif msg == "chunk_received" and seq is not None:
                    received.add(seq)
                elif msg == "ChunkSACK sent" and seq is not None:
                    sacked.add(seq)
                elif msg == "chunk_receive_failed" and seq is not None:
                    failed.add(seq)
                elif msg == "missing_chunk_in_assembly":
                    missing_seq = entry.get("missingSeq")
                    if isinstance(missing_seq, int):
                        explicit_missing.add(missing_seq)
                    elif isinstance(missing_seq, str):
                        try:
                            explicit_missing.add(int(missing_seq))
                        except ValueError:
                            pass
    except OSError as exc:
        print(f"Erro ao ler {log_path}: {exc}", file=sys.stderr)
        return 2

    if parsed_lines == 0:
        print("Nenhuma linha JSON valida encontrada no arquivo.", file=sys.stderr)
        return 2

    received_gaps = missing_ranges(received)
    started_without_received = started - received
    failed_without_received = failed - received
    received_without_sack = received - sacked if sacked else set()

    print(f"Arquivo: {log_path}")
    print(
        "Linhas lidas: "
        f"{parsed_lines} JSON validas, {invalid_lines} invalidas, {blank_lines} em branco"
    )

    if received:
        first_seq = min(received)
        last_seq = max(received)
        print(
            "chunk_received: "
            f"{len(received)} chunks unicos, faixa observada {first_seq}-{last_seq}"
        )
        if first_seq != 0:
            print(
                "Aviso: o primeiro globalSeq observado nao e 0; "
                "se o log estiver truncado, so os gaps no meio da faixa sao relevantes."
            )
    else:
        print("chunk_received: nenhum evento encontrado")

    problems_found = False

    if explicit_missing:
        problems_found = True
        ranges = values_to_ranges(explicit_missing)
        print(
            "ERRO explicito no log (missing_chunk_in_assembly): "
            f"{len(explicit_missing)} seqs -> {format_ranges(ranges)}"
        )

    if received_gaps:
        problems_found = True
        print(
            "Gaps em chunk_received dentro da faixa observada: "
            f"{count_range_items(received_gaps)} seqs faltando -> {format_ranges(received_gaps)}"
        )

    if started_without_received:
        problems_found = True
        ranges = values_to_ranges(started_without_received)
        print(
            "Chunks com chunk_receive_started mas sem chunk_received: "
            f"{len(started_without_received)} -> {format_ranges(ranges)}"
        )

    if failed_without_received:
        problems_found = True
        ranges = values_to_ranges(failed_without_received)
        print(
            "Chunks com chunk_receive_failed e sem recebimento posterior: "
            f"{len(failed_without_received)} -> {format_ranges(ranges)}"
        )

    if received_without_sack:
        ranges = values_to_ranges(received_without_sack)
        print(
            "Observacao: chunks recebidos sem ChunkSACK no arquivo: "
            f"{len(received_without_sack)} -> {format_ranges(ranges)}"
        )

    if not problems_found:
        print("Nenhum missing chunk evidente foi encontrado.")
        return 0

    return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
