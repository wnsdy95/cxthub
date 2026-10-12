#!/usr/bin/env python3
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("distribution", Path(__file__).with_name("distribute-repositories.py"))
distribution = importlib.util.module_from_spec(spec)
spec.loader.exec_module(distribution)


class DistributionTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="cxt-distribution-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.environment = patch.dict(os.environ, {
            "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null",
            "GIT_AUTHOR_NAME": "Export Test", "GIT_COMMITTER_NAME": "Export Test",
            "GIT_AUTHOR_EMAIL": "test@example.invalid", "GIT_COMMITTER_EMAIL": "test@example.invalid",
            "GIT_CONFIG_COUNT": "0",
        })
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.source = self.root / "source"
        self.source.mkdir()
        distribution.git(self.source, "init", "--template=", "-b", "main")
        config = {
            "version": 1, "source": distribution.SOURCE,
            "common": [["LICENSE", "LICENSE"]],
            "components": {name: {"repository": f"wnsdy95/cxthub-{name}", "visibility": visibility, "paths": [[name + "/", ""]]} for name, visibility in distribution.COMPONENTS.items()},
        }
        self.put("distrib/repositories.json", json.dumps(config))
        self.put("LICENSE", "synthetic license\n")
        for name in distribution.COMPONENTS:
            self.put(name + "/source.txt", name + " source\n")
        self.first = self.commit()
        self.remote = self.root / "remote.git"
        distribution.git(self.root, "init", "--bare", "--template=", "-b", "main", str(self.remote))

    def put(self, path, contents):
        target = self.source / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(contents)

    def commit(self):
        distribution.git(self.source, "add", "-f", "--all")
        distribution.git(self.source, "commit", "-m", "test source")
        return distribution.revision(self.source, "HEAD")

    def publish(self, commit):
        files = distribution.export_files(self.source, commit, "cli")
        return distribution.publish_component(self.source, commit, "cli", str(self.remote), files)

    def test_only_committed_allowlisted_files_and_stable_manifest(self):
        self.put("cli/source.txt", "dirty source")
        self.put("cli/.env", "private value")
        self.put(".cxt/session", "private context")
        files = distribution.export_files(self.source, self.first, "cli")
        self.assertEqual(files["source.txt"][1], b"cli source\n")
        self.assertEqual(set(files), {"source.txt", "LICENSE", distribution.MANIFEST})
        self.assertEqual(files, distribution.export_files(self.source, self.first, "cli"))
        manifest = json.loads(files[distribution.MANIFEST][1])
        self.assertEqual(manifest["source_commit"], self.first)
        self.assertEqual(manifest["component"], "cli")

    def test_rejects_tracked_secrets_and_symlinks(self):
        self.put("cli/.env", "fake private configuration")
        bad = self.commit()
        with self.assertRaisesRegex(distribution.DistributionError, "private/generated"):
            distribution.export_files(self.source, bad, "cli")
        (self.source / "cli/.env").unlink()
        (self.source / "cli/link").symlink_to("../LICENSE")
        bad = self.commit()
        with self.assertRaisesRegex(distribution.DistributionError, "symlink/submodule"):
            distribution.export_files(self.source, bad, "cli")

    def test_rejects_path_traversal_and_control_characters(self):
        for path in ["../oops", "/tmp/oops", "a/../oops", "a\\oops", "a\nb", "a//b", "./foo"]:
            with self.subTest(path=path), self.assertRaises(distribution.DistributionError):
                distribution.safe_path(path)

    def test_only_documented_mcp_template_is_allowed(self):
        distribution.safe_path("integrations/claude-code/.mcp.json")
        for path in [".mcp.json", "other/.mcp.json", "cli/.codex/config.toml"]:
            with self.subTest(path=path), self.assertRaises(distribution.DistributionError):
                distribution.safe_path(path)

    def test_mapping_collision_and_missing_input(self):
        config = json.loads((self.source / distribution.CONFIG).read_text())
        config["common"].append(["LICENSE", "source.txt"])
        self.put(distribution.CONFIG, json.dumps(config))
        with self.assertRaisesRegex(distribution.DistributionError, "duplicate"):
            distribution.export_files(self.source, self.commit(), "cli")
        config["common"] = [["missing", "LICENSE"]]
        self.put(distribution.CONFIG, json.dumps(config))
        with self.assertRaisesRegex(distribution.DistributionError, "missing required"):
            distribution.export_files(self.source, self.commit(), "cli")

    def test_publication_has_no_source_history_and_retry_is_noop(self):
        head = self.publish(self.first)
        self.assertEqual(distribution.git(self.remote, "rev-list", "--count", "main").strip(), b"1")
        self.assertEqual(self.publish(self.first), head)
        self.put("cli/source.txt", "new source\n")
        second = self.commit()
        new_head = self.publish(second)
        self.assertNotEqual(new_head, head)
        self.assertEqual(distribution.git(self.remote, "rev-list", "--count", "main").strip(), b"2")
        with self.assertRaises(distribution.DistributionError):
            self.publish(self.first)
        self.assertEqual(distribution.remote_head(self.source, str(self.remote)), new_head)

    def test_rejects_target_drift_even_when_manifest_is_changed(self):
        self.publish(self.first)
        checkout = self.root / "manual"
        distribution.git(self.root, "clone", "--template=", str(self.remote), str(checkout))
        (checkout / "source.txt").write_text("manual target edit")
        manifest = json.loads((checkout / distribution.MANIFEST).read_text())
        manifest["files"]["source.txt"]["sha256"] = "0" * 64
        (checkout / distribution.MANIFEST).write_text(json.dumps(manifest))
        distribution.git(checkout, "add", ".")
        distribution.git(checkout, "commit", "-m", "target drift")
        distribution.git(checkout, "push")
        with self.assertRaisesRegex(distribution.DistributionError, "target drift"):
            self.publish(self.first)

    def test_all_destinations_validated_before_any_publish(self):
        with patch.object(distribution, "check_source_eligibility"), patch.object(distribution, "check_destination", side_effect=distribution.DistributionError("visibility mismatch")), patch.object(distribution, "publish_component") as write:
            with self.assertRaises(distribution.DistributionError):
                distribution.publish(self.source, self.first)
            write.assert_not_called()

    def test_partial_failure_does_not_certify_release_set(self):
        with patch.object(distribution, "check_source_eligibility"), patch.object(distribution, "check_destination"), patch.object(distribution, "scan_exports"), patch.object(distribution, "remote_head", return_value=None), patch.object(distribution, "publish_component", side_effect=["1" * 40, distribution.DistributionError("network error")]) as write:
            with self.assertRaisesRegex(distribution.DistributionError, "network error"):
                distribution.publish(self.source, self.first)
            self.assertEqual(write.call_count, 2)

    def test_release_verification_requires_every_component_ci(self):
        files = distribution.export_files(self.source, self.first, "cli")
        config = distribution.config_at(self.source, self.first)
        record = {"id": 1, "head_sha": self.first, "event": "push", "head_branch": "main", "head_repository": {"full_name": "wnsdy95/cxthub-cli"}, "conclusion": "failure"}
        with patch.object(distribution, "config_at", return_value=config), patch.object(distribution, "check_destination"), patch.object(distribution, "git"), patch.object(distribution, "revision", return_value=self.first), patch.object(distribution, "remote_files", return_value=files), patch.object(distribution, "export_files", return_value=files), patch.object(distribution, "github", return_value={"workflow_runs": [record]}):
            with self.assertRaisesRegex(distribution.DistributionError, "CI is missing, pending or failed"):
                distribution.verify(self.source, self.first)

    def test_concurrent_target_update_is_not_overwritten(self):
        first_head = self.publish(self.first)
        self.put("cli/source.txt", "next source")
        second = self.commit()
        original_git = distribution.git
        concurrent_head = None

        def concurrent_push(root, *arguments, data=None):
            nonlocal concurrent_head
            if arguments[:1] == ("push",):
                previous_tree = original_git(self.remote, "rev-parse", f"{first_head}^{{tree}}").decode().strip()
                concurrent_head = original_git(self.remote, "commit-tree", previous_tree, "-p", first_head, data=b"Concurrent target change\n").decode().strip()
                original_git(self.remote, "update-ref", "refs/heads/main", concurrent_head, first_head)
            return original_git(root, *arguments, data=data)

        with patch.object(distribution, "git", side_effect=concurrent_push), self.assertRaises(distribution.DistributionError):
            self.publish(second)
        self.assertIsNotNone(concurrent_head)
        self.assertEqual(distribution.remote_head(self.source, str(self.remote)), concurrent_head)

    def test_concurrent_target_reset_or_deletion_is_not_overwritten(self):
        first_head = self.publish(self.first)
        self.put("cli/source.txt", "second source")
        second = self.commit()
        second_head = self.publish(second)
        self.put("cli/source.txt", "third source")
        third = self.commit()
        original_git = distribution.git
        for delete in [False, True]:
            original_git(self.remote, "update-ref", "refs/heads/main", second_head)

            def change_remote(root, *arguments, data=None):
                if arguments[:1] == ("push",):
                    if delete:
                        original_git(self.remote, "update-ref", "-d", "refs/heads/main", second_head)
                    else:
                        original_git(self.remote, "update-ref", "refs/heads/main", first_head, second_head)
                return original_git(root, *arguments, data=data)

            with self.subTest(delete=delete), patch.object(distribution, "git", side_effect=change_remote), self.assertRaises(distribution.DistributionError):
                self.publish(third)
            self.assertEqual(distribution.remote_head(self.source, str(self.remote)), None if delete else first_head)

    def test_superseded_source_after_preflight_never_writes(self):
        with patch.object(distribution, "check_source_eligibility", side_effect=[None, distribution.DistributionError("superseded")]), patch.object(distribution, "check_destination"), patch.object(distribution, "scan_exports"), patch.object(distribution, "remote_head", return_value=None), patch.object(distribution, "publish_component") as write:
            with self.assertRaisesRegex(distribution.DistributionError, "superseded"):
                distribution.publish(self.source, self.first)
            write.assert_not_called()

    def test_source_must_be_main_with_successful_push_ci(self):
        head = {"object": {"sha": self.first}}
        record = {"id": 1, "head_sha": self.first, "head_branch": "main", "event": "push", "head_repository": {"full_name": distribution.SOURCE}, "conclusion": "success"}
        with patch.object(distribution, "github", side_effect=[head, {"workflow_runs": [record]}]):
            distribution.check_source_eligibility(self.first)
        for change in [{"event": "pull_request"}, {"conclusion": "failure"}, {"head_repository": {"full_name": "untrusted/fork"}}]:
            with patch.object(distribution, "github", side_effect=[head, {"workflow_runs": [record | change]}]), self.assertRaises(distribution.DistributionError):
                distribution.check_source_eligibility(self.first)
        with patch.object(distribution, "github", return_value={"object": {"sha": "0" * 40}}), self.assertRaises(distribution.DistributionError):
            distribution.check_source_eligibility(self.first)

    def test_destination_visibility_is_exact(self):
        component = {"repository": "wnsdy95/cxthub-backend", "visibility": "private"}
        info = {"full_name": component["repository"], "visibility": "private", "archived": False, "fork": False, "default_branch": "main"}
        with patch.object(distribution, "github", return_value=info):
            distribution.check_destination(component)
        with patch.object(distribution, "github", return_value=info | {"visibility": "public"}), self.assertRaises(distribution.DistributionError):
            distribution.check_destination(component)

    def test_export_refuses_existing_destination(self):
        with self.assertRaises(FileExistsError):
            distribution.write_export(distribution.export_files(self.source, self.first, "cli"), self.source)

    def test_workflow_reconciles_main_instead_of_replaying_event_revision(self):
        root = Path(__file__).resolve().parent.parent
        workflow = (root / ".github/workflows/distribute.yml").read_text()
        self.assertEqual(workflow.count("ref: main"), 2)
        self.assertNotIn("ref: ${{ github.event.workflow_run.head_sha", workflow)
        self.assertIn("needs: eligible", workflow)
        self.assertIn("vars.REPOSITORY_SYNC_ENABLED == 'true'", workflow)
        self.assertIn("github.event.workflow_run.event == 'push'", workflow)
        self.assertIn("persist-credentials: false", workflow)


if __name__ == "__main__":
    unittest.main()
