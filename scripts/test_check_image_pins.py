"""Exercise image-pin discovery, release lines and the 24-hour quarantine."""

from __future__ import annotations

import datetime as dt
import importlib.util
import pathlib
import unittest

SPEC = importlib.util.spec_from_file_location(
    "check_image_pins", pathlib.Path(__file__).with_name("check-image-pins.py")
)
check = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(check)

NOW = check.parse_time("2026-10-11T12:00:00Z")
OLD = NOW - dt.timedelta(hours=24)
YOUNG = OLD + dt.timedelta(microseconds=1)
PINNED = "sha256:" + "a" * 64
MOVED = "sha256:" + "b" * 64


class ImagePinTests(unittest.TestCase):
    def test_pins_name_the_docker_hub_repository_they_copy(self) -> None:
        pins = check.find_pins(
            [
                ("Dockerfile", f"FROM public.ecr.aws/docker/library/golang:1.26.9@{PINNED} AS builder"),
                ("ci.yml", f"driver-opts: image=mirror.gcr.io/moby/buildkit:buildx-stable-1@{PINNED}"),
                ("run.sh", 'src="public.ecr.aws/docker/library/$img"'),
            ]
        )
        self.assertEqual(
            sorted((pin.hub, pin.tag) for pin, _ in pins.values()),
            [("library/golang", "1.26.9"), ("moby/buildkit", "buildx-stable-1")],
        )

    def test_newer_releases_stay_on_the_variant_and_release_line(self) -> None:
        candidates = [
            ("1.26.10-alpine", OLD),
            ("1.26.10", OLD),
            ("1.27.2-alpine", OLD),
            ("2.0.0-alpine", OLD),
            ("1.26-alpine", OLD),
        ]
        self.assertEqual(
            [tag for tag, _ in check.newer_releases("1.26.9-alpine", candidates)],
            ["1.27.2-alpine", "1.26.10-alpine"],
        )
        self.assertEqual(
            [tag for tag, _ in check.newer_releases("1.26.9-alpine", candidates, line=2)],
            ["1.26.10-alpine"],
        )
        self.assertEqual(check.newer_releases("26.04", [("26.10", OLD)], line=2), [])
        self.assertEqual(check.newer_releases("latest", candidates), [])

    def test_current_pin_served_by_the_mirror_is_ok(self) -> None:
        self.assertEqual(check.verdict(PINNED, PINNED, OLD, PINNED, [], NOW), ("ok", []))

    def test_moved_tag_is_held_for_24_hours_then_drifts(self) -> None:
        self.assertEqual(check.verdict(PINNED, MOVED, YOUNG, PINNED, [], NOW)[0], "HELD")
        self.assertEqual(check.verdict(PINNED, MOVED, OLD, PINNED, [], NOW)[0], "DRIFT")

    def test_newer_release_is_held_for_24_hours_then_drifts(self) -> None:
        self.assertEqual(check.verdict(PINNED, PINNED, OLD, PINNED, [("3.25", YOUNG)], NOW)[0], "HELD")
        self.assertEqual(check.verdict(PINNED, PINNED, OLD, PINNED, [("3.25", OLD)], NOW)[0], "DRIFT")

    def test_mirror_serving_another_digest_fails(self) -> None:
        self.assertEqual(check.verdict(PINNED, PINNED, OLD, MOVED, [], NOW)[0], "DRIFT")


if __name__ == "__main__":
    unittest.main()
