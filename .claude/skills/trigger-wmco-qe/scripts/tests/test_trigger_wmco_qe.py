from __future__ import annotations

import dataclasses
import importlib.util
import io
import json
import re
import shutil
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from unittest import mock

SCRIPT = Path(__file__).parents[1] / "trigger_wmco_qe.py"
SPEC = importlib.util.spec_from_file_location("trigger_wmco_qe", SCRIPT)
assert SPEC and SPEC.loader
wmco = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = wmco
SPEC.loader.exec_module(wmco)

FIXTURES = Path(__file__).parent / "fixtures"
RELEASE_FIXTURE = FIXTURES / "release"
REQUEST_TIME = "2026-10-07T12:00:00Z"
HEAD_SHA = "a" * 40
PR_NUMBER = 12345
FORK = "qe-bot/release"
ACTOR = "qe-bot"
RUN_ID_1 = "20261007t120000z-00000001"
RUN_ID_2 = "20261007t120001z-00000002"
VERSION = "4.99"
STREAM = "z"
TITLE = wmco.rehearsal_pr_title(VERSION, STREAM)

# The compact offline fixture deliberately uses synthetic workflow names. Its
# exact structural digests are injected only into this test process; the
# production manifest remains limited to audited public openshift/release
# workflows.
FIXTURE_TOPOLOGIES = {
    "wmco-connected-workflow": "66707ce30403e3f74a08c04567068ca7314f788af43593fc02f4014288294db4",
    "wmco-disconnected-workflow": "843d4104659802853ec9370133bebd19082d5a8d4017d506c99842b4f978bb3a",
}
wmco.AUDITED_WORKFLOW_TOPOLOGIES.update(FIXTURE_TOPOLOGIES)


def prow_url(job: str, build: str = "2107562700811800576") -> str:
    return (
        "https://qe-private-deck-ci.apps.ci.l2s4.p1.openshiftapps.com/"
        "view/gs/qe-private-deck/pr-logs/pull/openshift_release/"
        f"{PR_NUMBER}/rehearse-{PR_NUMBER}-{job}/{build}"
    )


def check_run(
    job: str,
    *,
    status: str = "completed",
    conclusion: str | None = "success",
    head_sha: str = HEAD_SHA,
    created_at: str = "2026-10-07T12:01:00Z",
    details_url: str | None = None,
    build: str = "2107562700811800576",
    app_slug: str = wmco.PROW_PUBLISHER_APP_SLUG,
) -> dict[str, object]:
    return {
        "name": f"ci/rehearse/{job}",
        "status": status,
        "conclusion": conclusion,
        "head_sha": head_sha,
        "created_at": created_at,
        "started_at": created_at,
        "details_url": details_url if details_url is not None else prow_url(job, build),
        "app": {"slug": app_slug},
        "source": "check_run",
    }


def commit_status(
    job: str,
    state: str,
    *,
    created_at: str,
    target_url: str | None,
    creator: str = wmco.AUTHORITATIVE_PROW_ACCOUNT,
) -> dict[str, object]:
    return {
        "context": f"ci/rehearse/{job}",
        "state": state,
        "created_at": created_at,
        "updated_at": created_at,
        "target_url": target_url,
        "creator": {"login": creator},
    }


def pr_snapshot(
    *,
    owner: str = "qe-bot",
    sha: str = HEAD_SHA,
    base: str = "main",
    title: str = TITLE,
) -> dict[str, object]:
    return {
        "number": PR_NUMBER,
        "url": f"https://github.com/openshift/release/pull/{PR_NUMBER}",
        "state": "OPEN",
        "isDraft": False,
        "baseRefName": base,
        "headRefName": "wmco-qe-test",
        "headRefOid": sha,
        "mergedAt": None,
        "title": title,
        "headRepository": {"name": "release", "nameWithOwner": f"{owner}/release"},
        "headRepositoryOwner": {"login": owner},
    }


def notifier_comment(body: str, *, comment_id: int = 1) -> dict[str, object]:
    return {
        "id": comment_id,
        "body": body,
        "html_url": f"https://github.com/openshift/release/pull/{PR_NUMBER}#issuecomment-{comment_id}",
        "user": {"login": wmco.NOTIFIER_LOGIN, "type": "Bot"},
        "performed_via_github_app": {"slug": wmco.NOTIFIER_APP_SLUG},
    }


def request_comment(
    command: str = "/pj-rehearse", *, comment_id: int = 2
) -> dict[str, object]:
    return {
        "id": comment_id,
        "body": command,
        "created_at": REQUEST_TIME,
        "html_url": f"https://github.com/openshift/release/pull/{PR_NUMBER}#issuecomment-{comment_id}",
        "issue_url": f"https://api.github.com/repos/openshift/release/issues/{PR_NUMBER}",
        "user": {"login": ACTOR, "type": "User"},
    }


class PlanningTests(unittest.TestCase):
    def copy_release(self, directory: str) -> Path:
        root = Path(directory) / "release"
        shutil.copytree(RELEASE_FIXTURE, root)
        return root

    def add_foreign_ref(self, root: Path, name: str, command_text: str) -> None:
        ref_dir = root / f"ci-operator/step-registry/test/wmco/foreign/{name}"
        ref_dir.mkdir(parents=True)
        (ref_dir / f"{name}-ref.yaml").write_text(
            f"ref:\n  as: {name}\n  from: cli\n  commands: {name}-commands.sh\n"
        )
        (ref_dir / f"{name}-commands.sh").write_text(command_text)

    def test_z_stream_selects_all_allowed_variants_and_disconnected_vsphere(
        self,
    ) -> None:
        plan = wmco.build_plan(RELEASE_FIXTURE, "4.99", "z-stream", base_sha=HEAD_SHA)
        self.assertEqual(
            plan.selected,
            (
                "aws-ipi-ovn-winc-f7",
                "aws-upi-ovn-winc-f7",
                "gcp-ipi-ovn-winc-f7",
                "azure-ipi-ovn-winc-f7",
                "vsphere-ipi-disconnected-ovn-winc-f14",
                "gcp-ipi-ovn-winc-f14-compliance",
                "gcp-ipi-ovn-winc-f14-compliance-destructive",
                "gcp-ipi-ovn-winc-f999-file-integrity",
            ),
        )
        self.assertNotIn("nutanix-ipi-ovn-winc-f28", plan.selected)
        self.assertEqual(
            plan.catalog_paths["wmco-disconnected-workflow"],
            wmco.DISCONNECTED_CATALOG_REF,
        )

    def test_y_stream_selects_every_suffix_bearing_member(self) -> None:
        plan = wmco.build_plan(RELEASE_FIXTURE, "4.99", "y")
        self.assertEqual(plan.selected, plan.discovered)
        self.assertIn("gcp-ipi-ovn-winc-f14-compliance", plan.selected)
        self.assertIn("gcp-ipi-ovn-winc-f999-file-integrity", plan.selected)
        self.assertIn("nutanix-ipi-ovn-winc-f28", plan.selected)

    def test_non_periodic_and_debug_winc_entries_are_excluded(self) -> None:
        config = (
            "tests:\n"
            "- as: aws-ipi-ovn-winc-f7\n"
            "  steps: {}\n"
            "- as: aws-ipi-ovn-winc-f7-debug\n"
            "  cron: 0 0 31 2 *\n"
            "  steps: {}\n"
            "- as: aws-upi-ovn-winc-f7\n"
            "  cron: 0 0 31 2 *\n"
            "  steps: {}\n"
        )
        self.assertEqual(wmco.discover_wmco_jobs(config), ("aws-upi-ovn-winc-f7",))

    def test_requested_minor_must_match_candidate_payload(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            config = next(root.glob("**/*release-4.99__amd64-nightly.yaml"))
            config.write_text(
                config.read_text().replace('version: "4.99"', 'version: "4.98"', 1)
            )
            with self.assertRaisesRegex(wmco.SafetyError, "does not match"):
                wmco.build_plan(root, "4.99", "z")

    def test_supported_indirect_release_payload_is_linked(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            config = next(root.glob("**/*release-4.99__amd64-nightly.yaml"))
            config.write_text(
                config.read_text().replace(
                    '    candidate:\n      product: ocp\n      stream: nightly\n      version: "4.99"',
                    '    release:\n      channel: nightly\n      version: "4.99"',
                    1,
                )
            )
            plan = wmco.build_plan(root, "4.99", "z")
            self.assertEqual(plan.payload_definition, "release:4.99")

    def test_decoy_latest_assignment_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            command = next(
                root.glob("**/openshift-windows-setup-wmco-konflux-commands.sh")
            )
            command.write_text(
                command.read_text().replace(
                    '  echo "$image_url"',
                    '  image_url="quay.io/decoy:latest"\n  echo "$image_url"',
                )
            )
            with self.assertRaisesRegex(
                wmco.SafetyError, "unsupported .* template shape"
            ):
                wmco.build_plan(root, "4.99", "z")

    def test_later_ocp_tag_reassignment_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            command = next(
                root.glob("**/openshift-windows-setup-wmco-konflux-commands.sh")
            )
            assignment = '  ocp_tag="release-${version//./-}"'
            command.write_text(
                command.read_text().replace(
                    assignment,
                    assignment + '\n  ocp_tag="release-foreign"',
                )
            )
            with self.assertRaisesRegex(
                wmco.SafetyError, "unsupported .* template shape"
            ):
                wmco.build_plan(root, "4.99", "z")

    def test_commented_latest_image_echo_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            command = next(
                root.glob("**/openshift-windows-setup-wmco-konflux-commands.sh")
            )
            command.write_text(
                command.read_text().replace(
                    '  echo "$image_url"',
                    '  # echo "$image_url"\n'
                    '  cat <<DECOY > "${ARTIFACT_DIR}/decoy"\n'
                    'echo "$image_url"\n'
                    "DECOY",
                )
            )
            with self.assertRaisesRegex(
                wmco.SafetyError, "unsupported .* template shape"
            ):
                wmco.build_plan(root, "4.99", "z")

    def test_altered_catalog_dataflow_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            command = next(
                root.glob("**/openshift-windows-setup-wmco-konflux-commands.sh")
            )
            command.write_text(
                command.read_text().replace(
                    "wmco_index_image=$(get_latest_wmco_index_image)",
                    'wmco_index_image="quay.io/decoy:latest"',
                )
            )
            with self.assertRaisesRegex(
                wmco.SafetyError, "unsupported .* template shape"
            ):
                wmco.build_plan(root, "4.99", "z")

    def test_later_connected_catalog_reassignment_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            command = next(
                root.glob("**/openshift-windows-setup-wmco-konflux-commands.sh")
            )
            assignment = "wmco_index_image=$(get_latest_wmco_index_image)"
            command.write_text(
                command.read_text().replace(
                    assignment,
                    assignment + '\nwmco_index_image="quay.io/foreign/catalog:stable"',
                )
            )
            with self.assertRaisesRegex(
                wmco.SafetyError, "unsupported .* template shape"
            ):
                wmco.build_plan(root, "4.99", "z")

    def test_job_subscription_override_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            config = next(root.glob("**/*release-4.99__amd64-nightly.yaml"))
            config.write_text(
                config.read_text().replace(
                    "  steps:\n    workflow: wmco-connected-workflow",
                    "  steps:\n    env:\n      SUB_SOURCE: decoy\n    workflow: wmco-connected-workflow",
                    1,
                )
            )
            with self.assertRaisesRegex(wmco.SafetyError, "override"):
                wmco.build_plan(root, "4.99", "z")

    def test_closure_subscription_override_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            workflow = next(root.glob("**/wmco-connected-workflow-workflow.yaml"))
            workflow.write_text(
                workflow.read_text().replace(
                    'SUB_SOURCE: "wmco"', 'SUB_SOURCE: "decoy"'
                )
            )
            with self.assertRaisesRegex(wmco.SafetyError, "SUB_SOURCE"):
                wmco.build_plan(root, "4.99", "z")

    def test_subscription_values_must_come_from_workflow_env(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            workflow = next(root.glob("**/wmco-connected-workflow-workflow.yaml"))
            text = workflow.read_text()
            for line in (
                "      SUB_CHANNEL: stable\n",
                "      SUB_PACKAGE: windows-machine-config-operator\n",
                '      SUB_SOURCE: "wmco"\n',
            ):
                text = text.replace(line, "")
            workflow.write_text(text)
            chain = next(root.glob("**/wmco-provision-chain.yaml"))
            chain.write_text(
                chain.read_text()
                + "  documentation: |-\n"
                + "    SUB_CHANNEL: stable\n"
                + "    SUB_PACKAGE: windows-machine-config-operator\n"
                + "    SUB_SOURCE: wmco\n"
            )
            with self.assertRaisesRegex(
                wmco.SafetyError, "steps.env must be a mapping"
            ):
                wmco.build_plan(root, "4.99", "z")

    def test_catalog_manifest_must_be_applied_after_final_image_use(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            command = next(
                root.glob("**/openshift-windows-setup-wmco-konflux-commands.sh")
            )
            command.write_text(
                command.read_text().replace(
                    '  oc apply -f "${ARTIFACT_DIR}/wmco_catalogsource.yaml"',
                    '  # oc apply -f "${ARTIFACT_DIR}/wmco_catalogsource.yaml"\n'
                    '  cat <<DECOY > "${ARTIFACT_DIR}/decoy"\n'
                    'oc apply -f "${ARTIFACT_DIR}/wmco_catalogsource.yaml"\n'
                    "DECOY",
                )
            )
            with self.assertRaisesRegex(
                wmco.SafetyError, "unsupported .* template shape"
            ):
                wmco.build_plan(root, "4.99", "z")

    def test_connected_and_disconnected_catalog_producers_match_applied_files(
        self,
    ) -> None:
        plan = wmco.build_plan(RELEASE_FIXTURE, "4.99", "z")
        self.assertEqual(
            set(plan.catalog_paths.values()),
            {wmco.CONNECTED_CATALOG_REF, wmco.DISCONNECTED_CATALOG_REF},
        )

    def test_catalog_producer_redirect_to_unapplied_decoy_is_rejected(self) -> None:
        cases = (
            (
                "**/openshift-windows-setup-wmco-konflux-commands.sh",
                '"${ARTIFACT_DIR}/wmco_catalogsource.yaml"',
            ),
            (
                "**/openshift-windows-setup-wmco-konflux-disconnected-commands.sh",
                '"${ARTIFACT_DIR}/wmco-catalogsource-disconnected.yaml"',
            ),
        )
        for pattern, target in cases:
            with (
                self.subTest(pattern=pattern),
                tempfile.TemporaryDirectory() as directory,
            ):
                root = self.copy_release(directory)
                command = next(root.glob(pattern))
                command.write_text(
                    command.read_text().replace(
                        f"cat <<EOF > {target}", "cat <<EOF > unapplied-decoy.yaml"
                    )
                )
                with self.assertRaisesRegex(
                    wmco.SafetyError, "unsupported .* template shape"
                ):
                    wmco.build_plan(root, "4.99", "z")

    def test_catalog_file_replacement_before_apply_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            command = next(
                root.glob("**/openshift-windows-setup-wmco-konflux-commands.sh")
            )
            apply = '  oc apply -f "${ARTIFACT_DIR}/wmco_catalogsource.yaml"'
            command.write_text(
                command.read_text().replace(
                    apply,
                    '  printf decoy > "${ARTIFACT_DIR}/wmco_catalogsource.yaml"\n'
                    + apply,
                )
            )
            with self.assertRaisesRegex(
                wmco.SafetyError, "unsupported .* template shape"
            ):
                wmco.build_plan(root, "4.99", "z")

    def test_vetted_producers_reject_sed_replacement_and_second_apply(self) -> None:
        cases = (
            (
                "**/openshift-windows-setup-wmco-konflux-commands.sh",
                '  oc apply -f "${ARTIFACT_DIR}/wmco_catalogsource.yaml"',
                (
                    "  sed -i 's#${wmco_index_image}#quay.io/foreign/catalog:stable#' "
                    '"${ARTIFACT_DIR}/wmco_catalogsource.yaml"\n'
                ),
                '  oc apply -f "${ARTIFACT_DIR}/foreign-wmco-catalogsource.yaml"\n',
            ),
            (
                "**/openshift-windows-setup-wmco-konflux-disconnected-commands.sh",
                '  run_command "oc apply -f ${ARTIFACT_DIR}/wmco-catalogsource-disconnected.yaml"',
                (
                    "  sed -i 's#${mirrored_index}#quay.io/foreign/catalog:stable#' "
                    '"${ARTIFACT_DIR}/wmco-catalogsource-disconnected.yaml"\n'
                ),
                '  run_command "oc apply -f ${ARTIFACT_DIR}/foreign-wmco-catalogsource.yaml"\n',
            ),
        )
        for pattern, apply, sed_mutation, foreign_apply in cases:
            for mutation in (sed_mutation, foreign_apply):
                with (
                    self.subTest(pattern=pattern, mutation=mutation.strip()),
                    tempfile.TemporaryDirectory() as directory,
                ):
                    root = self.copy_release(directory)
                    command = next(root.glob(pattern))
                    command.write_text(
                        command.read_text().replace(apply, mutation + apply, 1)
                    )
                    with self.assertRaisesRegex(
                        wmco.SafetyError, "unsupported .* template shape"
                    ):
                        wmco.build_plan(root, "4.99", "z")

    def test_unknown_direct_stdin_catalog_producer_shape_is_rejected(self) -> None:
        cases = (
            (
                "**/openshift-windows-setup-wmco-konflux-commands.sh",
                'cat <<EOF > "${ARTIFACT_DIR}/wmco_catalogsource.yaml"',
                '  oc apply -f "${ARTIFACT_DIR}/wmco_catalogsource.yaml"\n',
            ),
            (
                "**/openshift-windows-setup-wmco-konflux-disconnected-commands.sh",
                'cat <<EOF > "${ARTIFACT_DIR}/wmco-catalogsource-disconnected.yaml"',
                '  run_command "oc apply -f ${ARTIFACT_DIR}/wmco-catalogsource-disconnected.yaml"\n',
            ),
        )
        for pattern, opener, apply in cases:
            with (
                self.subTest(pattern=pattern),
                tempfile.TemporaryDirectory() as directory,
            ):
                root = self.copy_release(directory)
                command = next(root.glob(pattern))
                text = command.read_text().replace(opener, "cat <<EOF | oc apply -f -")
                command.write_text(text.replace(apply, ""))
                with self.assertRaisesRegex(
                    wmco.SafetyError, "unsupported .* template shape"
                ):
                    wmco.build_plan(root, "4.99", "z")

    def test_operatorhub_subscription_manifest_must_be_applied(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            command = next(root.glob("**/operatorhub-subscribe-commands.sh"))
            command.write_text(
                command.read_text().replace("cat <<EOF | oc apply -f -", "cat <<EOF")
            )
            with self.assertRaisesRegex(
                wmco.SafetyError, "unsupported .* template shape"
            ):
                wmco.build_plan(root, "4.99", "z")

    def test_operatorhub_consumer_cannot_mutate_injected_subscription_values(
        self,
    ) -> None:
        mutations = (
            "SUB_SOURCE=redhat-operators",
            "SUB_CHANNEL=fast",
            "unset SUB_PACKAGE",
        )
        for mutation in mutations:
            with (
                self.subTest(mutation=mutation),
                tempfile.TemporaryDirectory() as directory,
            ):
                root = self.copy_release(directory)
                command = next(root.glob("**/operatorhub-subscribe-commands.sh"))
                command.write_text(
                    command.read_text().replace(
                        "cat <<EOF | oc apply -f -",
                        f"{mutation}\n\ncat <<EOF | oc apply -f -",
                    )
                )
                with self.assertRaisesRegex(
                    wmco.SafetyError, "unsupported .* template shape"
                ):
                    wmco.build_plan(root, "4.99", "z")

    def test_operatorhub_consumer_rejects_indirect_subscription_mutation(self) -> None:
        mutations = (
            (
                "declare -n subscription_target=SUB_SOURCE\n"
                "subscription_target=redhat-operators"
            ),
            "SUB_SOURCE[0]=redhat-operators",
        )
        for mutation in mutations:
            with (
                self.subTest(mutation=mutation),
                tempfile.TemporaryDirectory() as directory,
            ):
                root = self.copy_release(directory)
                command = next(root.glob("**/operatorhub-subscribe-commands.sh"))
                command.write_text(
                    command.read_text().replace(
                        "cat <<EOF | oc apply -f -",
                        f"{mutation}\n\ncat <<EOF | oc apply -f -",
                        1,
                    )
                )
                with self.assertRaisesRegex(
                    wmco.SafetyError, "unsupported .* template shape"
                ):
                    wmco.build_plan(root, "4.99", "z")

    def test_catalog_assignment_in_decoy_heredoc_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            command = next(
                root.glob("**/openshift-windows-setup-wmco-konflux-commands.sh")
            )
            assignment = "  wmco_index_image=$(get_latest_wmco_index_image)"
            command.write_text(
                command.read_text().replace(
                    assignment,
                    "  # wmco_index_image=$(get_latest_wmco_index_image)\n"
                    '  cat <<DECOY > "${ARTIFACT_DIR}/decoy"\n'
                    "wmco_index_image=$(get_latest_wmco_index_image)\n"
                    "DECOY",
                )
            )
            with self.assertRaisesRegex(
                wmco.SafetyError, "unsupported .* template shape"
            ):
                wmco.build_plan(root, "4.99", "z")

    def test_competing_catalog_setup_path_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            chain = next(root.glob("**/wmco-provision-chain.yaml"))
            chain.write_text(
                chain.read_text()
                + "  - ref: openshift-windows-setup-wmco-konflux-disconnected\n"
            )
            with self.assertRaisesRegex(wmco.SafetyError, "phase/order topology"):
                wmco.build_plan(root, "4.99", "z")

    def test_arbitrary_named_catalog_writer_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            writer_dir = (
                root / "ci-operator/step-registry/test/wmco/foreign/catalog-writer"
            )
            writer_dir.mkdir(parents=True)
            (writer_dir / "foreign-catalog-writer-ref.yaml").write_text(
                "ref:\n"
                "  as: foreign-catalog-writer\n"
                "  from: cli\n"
                "  commands: foreign-catalog-writer-commands.sh\n"
            )
            (writer_dir / "foreign-catalog-writer-commands.sh").write_text(
                "#!/bin/bash\n"
                "cat <<EOF | oc apply -f -\n"
                "apiVersion: operators.coreos.com/v1alpha1\n"
                "kind: CatalogSource\n"
                "metadata:\n"
                "  name: wmco\n"
                "spec:\n"
                "  image: quay.io/foreign/catalog:stable\n"
                "EOF\n"
            )
            chain = next(root.glob("**/wmco-provision-chain.yaml"))
            chain.write_text(chain.read_text() + "  - ref: foreign-catalog-writer\n")
            with self.assertRaisesRegex(wmco.SafetyError, "phase/order topology"):
                wmco.build_plan(root, "4.99", "z")

    def test_direct_catalogsource_patch_in_arbitrary_ref_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            writer_dir = root / "ci-operator/step-registry/test/wmco/direct-patch"
            writer_dir.mkdir(parents=True)
            (writer_dir / "direct-patch-ref.yaml").write_text(
                "ref:\n"
                "  as: direct-patch\n"
                "  from: cli\n"
                "  commands: direct-patch-commands.sh\n"
            )
            (writer_dir / "direct-patch-commands.sh").write_text(
                "#!/bin/bash\n"
                "oc patch catalogsource wmco -n openshift-marketplace --type=merge "
                '-p \'{"spec":{"image":"quay.io/foreign/catalog:stable"}}\'\n'
            )
            chain = next(root.glob("**/wmco-provision-chain.yaml"))
            chain.write_text(chain.read_text() + "  - ref: direct-patch\n")
            with self.assertRaisesRegex(wmco.SafetyError, "phase/order topology"):
                wmco.build_plan(root, "4.99", "z")

    def test_catalogsource_resource_alias_mutations_are_rejected(self) -> None:
        resources = (
            "catalogsource wmco",
            "catalogsources wmco",
            "catalogsource.operators.coreos.com wmco",
            "catalogsources.operators.coreos.com wmco",
            "catsrc wmco",
            "catsrc/wmco",
        )
        for resource in resources:
            with (
                self.subTest(resource=resource),
                tempfile.TemporaryDirectory() as directory,
            ):
                root = self.copy_release(directory)
                writer_dir = root / "ci-operator/step-registry/test/wmco/alias-patch"
                writer_dir.mkdir(parents=True)
                (writer_dir / "alias-patch-ref.yaml").write_text(
                    "ref:\n"
                    "  as: alias-patch\n"
                    "  from: cli\n"
                    "  commands: alias-patch-commands.sh\n"
                )
                (writer_dir / "alias-patch-commands.sh").write_text(
                    "#!/bin/bash\n"
                    f"oc patch {resource} -n openshift-marketplace --type=merge "
                    '-p \'{"spec":{"image":"quay.io/foreign/catalog:stable"}}\'\n'
                )
                chain = next(root.glob("**/wmco-provision-chain.yaml"))
                chain.write_text(chain.read_text() + "  - ref: alias-patch\n")
                with self.assertRaisesRegex(wmco.SafetyError, "phase/order topology"):
                    wmco.build_plan(root, "4.99", "z")

    def test_interspersed_catalogsource_flags_in_arbitrary_ref_are_rejected(
        self,
    ) -> None:
        commands = (
            (
                "oc patch catsrc -n openshift-marketplace wmco --type=merge "
                '-p \'{"spec":{"image":"quay.io/foreign/catalog:stable"}}\''
            ),
            (
                "oc patch -n openshift-marketplace "
                "catalogsources.operators.coreos.com/wmco --type=merge "
                '-p \'{"spec":{"image":"quay.io/foreign/catalog:stable"}}\''
            ),
        )
        for command_text in commands:
            with (
                self.subTest(command=command_text),
                tempfile.TemporaryDirectory() as directory,
            ):
                root = self.copy_release(directory)
                writer_dir = root / "ci-operator/step-registry/test/wmco/flag-patch"
                writer_dir.mkdir(parents=True)
                (writer_dir / "flag-patch-ref.yaml").write_text(
                    "ref:\n"
                    "  as: flag-patch\n"
                    "  from: cli\n"
                    "  commands: flag-patch-commands.sh\n"
                )
                (writer_dir / "flag-patch-commands.sh").write_text(
                    f"#!/bin/bash\n{command_text}\n"
                )
                chain = next(root.glob("**/wmco-provision-chain.yaml"))
                chain.write_text(chain.read_text() + "  - ref: flag-patch\n")
                with self.assertRaisesRegex(wmco.SafetyError, "phase/order topology"):
                    wmco.build_plan(root, "4.99", "z")

    def test_block_inline_and_commented_dependencies_parse_identically(self) -> None:
        block = (
            "chain:\n"
            "  as: example\n"
            "  steps:\n"
            "  - ref: first\n"
            "  - chain: second # retained dependency\n"
        )
        inline = (
            "chain:\n"
            "  as: example\n"
            "  steps:\n"
            "  - {ref: first}\n"
            "  - {chain: second} # retained dependency\n"
        )
        expected = (("ref", "first"), ("chain", "second"))
        self.assertEqual(wmco.registry_dependencies(block), expected)
        self.assertEqual(wmco.registry_dependencies(inline), expected)

    def test_dependency_shapes_fail_closed(self) -> None:
        entries = (
            "  - scalar\n",
            "  - {ref: first, chain: second}\n",
            "  - {ref: first, invented: value}\n",
        )
        for entry in entries:
            with self.subTest(entry=entry):
                document = "chain:\n  as: example\n  steps:\n" + entry
                with self.assertRaises(wmco.SafetyError):
                    wmco.registry_dependencies(document)
        malformed_documents = (
            "chain: scalar\n",
            "chain:\n  as: example\n  steps: []\n  invented: value\n",
            "chain:\n  as: example\n  steps: []\nworkflow:\n  as: other\n  steps: {}\n",
        )
        for document in malformed_documents:
            with self.subTest(document=document), self.assertRaises(wmco.SafetyError):
                wmco.registry_dependencies(document)

    def test_duplicate_registry_keys_and_yaml_merges_fail_closed(self) -> None:
        documents = {
            "inline dependency": (
                "chain:\n  as: example\n  steps:\n  - {ref: first, ref: second}\n"
            ),
            "block dependency": (
                "chain:\n  as: example\n  steps:\n  - ref: first\n    ref: second\n"
            ),
            "nested node": (
                "workflow:\n  as: example\n  steps:\n    env:\n"
                "      SHARED: first\n      SHARED: second\n"
            ),
            "top-level node": (
                "chain:\n  as: first\n  steps: []\nchain:\n  as: second\n  steps: []\n"
            ),
            "merge key": (
                "chain:\n  as: example\n  defaults: &defaults\n    ref: first\n"
                "  steps:\n  - <<: *defaults\n"
            ),
        }
        for description, document in documents.items():
            with (
                self.subTest(description=description),
                self.assertRaisesRegex(wmco.SafetyError, "ambiguous YAML"),
            ):
                wmco.registry_dependencies(document)

    def test_duplicate_release_config_key_fails_closed(self) -> None:
        config = (
            "tests:\n"
            "- as: aws-ipi-ovn-winc-f7\n"
            "  cron: 0 0 31 2 *\n"
            "  steps: {}\n"
            "tests: []\n"
        )
        with self.assertRaisesRegex(wmco.SafetyError, "ambiguous YAML"):
            wmco.discover_wmco_jobs(config)

    def test_unsafe_yaml_python_tag_fails_closed(self) -> None:
        with self.assertRaisesRegex(wmco.SafetyError, "invalid or ambiguous YAML"):
            wmco.strict_yaml_load(
                "value: !!python/object/apply:os.system ['echo unsafe']\n",
                context="test document",
            )

    def test_yaml_reader_errors_fail_closed_as_safety_errors(self) -> None:
        with self.assertRaisesRegex(wmco.SafetyError, "invalid or ambiguous YAML"):
            wmco.strict_yaml_load("value: \x00\n", context="test document")

    def test_release_config_symlink_cannot_escape_checkout(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            config = next(root.glob("**/*release-4.99__amd64-nightly.yaml"))
            outside = Path(directory) / "outside.yaml"
            outside.write_text(config.read_text())
            config.unlink()
            config.symlink_to(outside)
            with self.assertRaisesRegex(wmco.SafetyError, "escapes"):
                wmco.build_plan(root, "4.99", "z")

    def test_full_plan_rejects_inline_foreign_dependency(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            self.add_foreign_ref(
                root,
                "inline-foreign",
                "#!/bin/bash\noc patch catsrc -n openshift-marketplace wmco\n",
            )
            chain = next(root.glob("**/wmco-provision-chain.yaml"))
            chain.write_text(chain.read_text() + "  - {ref: inline-foreign}\n")
            with self.assertRaisesRegex(wmco.SafetyError, "phase/order topology"):
                wmco.build_plan(root, "4.99", "z")

    def test_full_plan_rejects_multiline_foreign_script_even_if_topology_is_trusted(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            self.add_foreign_ref(
                root,
                "multiline-foreign",
                "#!/bin/bash\n"
                "oc patch catsrc wmco -n openshift-marketplace --type=merge -p '{\n"
                '  "spec":{"image":"quay.io/foreign/catalog:stable"}\n'
                "}'\n",
            )
            chain = next(root.glob("**/wmco-provision-chain.yaml"))
            chain.write_text(chain.read_text() + "  - ref: multiline-foreign\n")
            graph = wmco.registry_closure(root, "wmco-connected-workflow")
            trusted_topology = dict(wmco.AUDITED_WORKFLOW_TOPOLOGIES)
            trusted_topology["wmco-connected-workflow"] = graph.topology_digest
            with (
                mock.patch.object(
                    wmco, "AUDITED_WORKFLOW_TOPOLOGIES", trusted_topology
                ),
                self.assertRaisesRegex(wmco.SafetyError, "executable closure command"),
            ):
                wmco.build_plan(root, "4.99", "z")

    def test_trusted_roles_must_remain_in_pre_provision_topology(self) -> None:
        cases = (
            ("wmco-provision-chain.yaml", wmco.CONNECTED_CATALOG_REF),
            ("wmco-provision-chain.yaml", wmco.OPERATORHUB_SUBSCRIBE_REF),
            (
                "wmco-disconnected-provision-chain.yaml",
                wmco.DISCONNECTED_CATALOG_REF,
            ),
            ("wmco-disconnected-provision-chain.yaml", wmco.DISCONNECTED_INSTALL_REF),
        )
        for chain_name, role in cases:
            for phase in ("test", "post"):
                with (
                    self.subTest(chain=chain_name, role=role, phase=phase),
                    tempfile.TemporaryDirectory() as directory,
                ):
                    root = self.copy_release(directory)
                    chain = next(root.glob(f"**/{chain_name}"))
                    chain.write_text(
                        chain.read_text().replace(f"  - ref: {role}\n", "")
                    )
                    workflow_name = (
                        "wmco-disconnected-workflow"
                        if "disconnected" in chain_name
                        else "wmco-connected-workflow"
                    )
                    workflow = wmco.unique_registry_file(
                        root, workflow_name, "workflow"
                    )
                    workflow.write_text(
                        workflow.read_text() + f"    {phase}:\n    - ref: {role}\n"
                    )
                    with self.assertRaisesRegex(
                        wmco.SafetyError, "phase/order topology"
                    ):
                        wmco.build_plan(root, "4.99", "z")

    def test_manifest_identity_topology_and_inventory_are_plan_bound(self) -> None:
        plan = wmco.build_plan(RELEASE_FIXTURE, "4.99", "z")
        for workflow, manifest in plan.closure_manifests.items():
            self.assertEqual(manifest["identity"], wmco.CLOSURE_MANIFEST_IDENTITY)
            self.assertEqual(manifest["schema"], str(wmco.CLOSURE_MANIFEST_SCHEMA))
            self.assertEqual(
                manifest["topology_digest"],
                wmco.AUDITED_WORKFLOW_TOPOLOGIES[workflow],
            )
            self.assertEqual(
                plan.safety_inputs[f"manifest:{workflow}:topology"],
                manifest["topology_digest"],
            )
            self.assertEqual(
                plan.safety_inputs[f"manifest:{workflow}:commands"],
                manifest["command_inventory_digest"],
            )
        changed = dict(plan.closure_manifests)
        workflow = next(iter(changed))
        changed[workflow] = dict(changed[workflow], identity="drifted")
        self.assertNotEqual(
            plan.digest, dataclasses.replace(plan, closure_manifests=changed).digest
        )
        output = io.StringIO()
        with redirect_stdout(output):
            wmco.print_plan(plan)
        self.assertIn(wmco.CLOSURE_MANIFEST_IDENTITY, output.getvalue())
        self.assertIn("topology=sha256:", output.getvalue())
        self.assertIn("commands=sha256:", output.getvalue())

    def test_public_template_identity_digest_and_narrow_normalization(self) -> None:
        plan = wmco.build_plan(RELEASE_FIXTURE, "4.99", "z")
        records = [
            record
            for workflow_records in plan.catalog_templates.values()
            for record in workflow_records
        ]
        self.assertEqual(
            {record["role"] for record in records},
            {
                "connected-producer",
                "connected-consumer",
                "disconnected-producer",
                "disconnected-consumer",
            },
        )
        self.assertTrue(
            all(
                record["identity"].startswith(wmco.PUBLIC_TEMPLATE_SOURCE)
                for record in records
            )
        )
        self.assertTrue(
            all(re.fullmatch(r"[0-9a-f]{64}", record["digest"]) for record in records)
        )
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            for command in root.glob("**/*-commands.sh"):
                if command.name in {Path(record["path"]).name for record in records}:
                    lines = command.read_text().splitlines()
                    command.write_text(
                        "\r\n".join(line + "  " for line in lines) + "\r\n"
                    )
            wmco.build_plan(root, "4.99", "z")

    def test_connected_subscription_consumer_is_required(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            chain = next(root.glob("**/wmco-provision-chain.yaml"))
            chain.write_text(
                chain.read_text().replace("  - ref: operatorhub-subscribe\n", "")
            )
            with self.assertRaisesRegex(wmco.SafetyError, "phase/order topology"):
                wmco.build_plan(root, "4.99", "z")

    def test_disconnected_digest_mirror_and_subscription_are_required(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            install = next(root.glob("**/openshift-windows-install-wmco-commands.sh"))
            install.write_text(
                install.read_text().replace("source: wmco", "source: redhat-operators")
            )
            with self.assertRaisesRegex(
                wmco.SafetyError, "unsupported .* template shape"
            ):
                wmco.build_plan(root, "4.99", "z")

    def test_later_disconnected_mirror_reassignment_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            command = next(
                root.glob(
                    "**/openshift-windows-setup-wmco-konflux-disconnected-commands.sh"
                )
            )
            assignment = (
                '  local mirrored_index="${mirrored_index_repo}@${wmco_index_digest}"'
            )
            command.write_text(
                command.read_text().replace(
                    assignment,
                    assignment + '\n  mirrored_index="quay.io/foreign/catalog:stable"',
                )
            )
            with self.assertRaisesRegex(
                wmco.SafetyError, "unsupported .* template shape"
            ):
                wmco.build_plan(root, "4.99", "z")

    def test_disconnected_mirror_and_catalog_apply_must_be_executable(self) -> None:
        mutations = (
            (
                '  until oc image mirror "${wmco_index_with_digest}=${mirrored_index_repo}"',
                '  # until oc image mirror "${wmco_index_with_digest}=${mirrored_index_repo}"',
                "executable digest mirror",
            ),
            (
                '  run_command "oc apply -f ${ARTIFACT_DIR}/wmco-catalogsource-disconnected.yaml"',
                '  # run_command "oc apply -f ${ARTIFACT_DIR}/wmco-catalogsource-disconnected.yaml"',
                "executable disconnected CatalogSource apply",
            ),
        )
        for original, replacement, message in mutations:
            with (
                self.subTest(message=message),
                tempfile.TemporaryDirectory() as directory,
            ):
                root = self.copy_release(directory)
                command = next(
                    root.glob(
                        "**/openshift-windows-setup-wmco-konflux-disconnected-commands.sh"
                    )
                )
                command.write_text(command.read_text().replace(original, replacement))
                with self.assertRaisesRegex(
                    wmco.SafetyError, "unsupported .* template shape"
                ):
                    wmco.build_plan(root, "4.99", "z")

    def test_disconnected_dynamic_subscription_manifest_must_be_applied(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_release(directory)
            install = next(root.glob("**/openshift-windows-install-wmco-commands.sh"))
            install.write_text(
                install.read_text().replace("cat <<EOF | oc apply -f -", "cat <<EOF", 1)
            )
            with self.assertRaisesRegex(
                wmco.SafetyError, "unsupported .* template shape"
            ):
                wmco.build_plan(root, "4.99", "z")

    def test_catalog_setup_functions_must_be_invoked(self) -> None:
        cases = (
            (
                "**/openshift-windows-setup-wmco-konflux-commands.sh",
                "setup_wmco_catalog\n",
                "# setup_wmco_catalog\n",
                "connected catalog setup invocation",
            ),
            (
                "**/openshift-windows-setup-wmco-konflux-disconnected-commands.sh",
                "setup_wmco_catalog_disconnected\n",
                "# setup_wmco_catalog_disconnected\n",
                "disconnected catalog setup invocation",
            ),
        )
        for pattern, original, replacement, message in cases:
            with (
                self.subTest(message=message),
                tempfile.TemporaryDirectory() as directory,
            ):
                root = self.copy_release(directory)
                command = next(root.glob(pattern))
                command.write_text(command.read_text().replace(original, replacement))
                with self.assertRaisesRegex(
                    wmco.SafetyError, "unsupported .* template shape"
                ):
                    wmco.build_plan(root, "4.99", "z")

    def test_job_plan_enforces_1_to_25_before_lifecycle(self) -> None:
        plan = wmco.build_plan(RELEASE_FIXTURE, "4.99", "z")
        with self.assertRaisesRegex(wmco.SafetyError, "empty"):
            dataclasses.replace(plan, selected=())
        jobs = tuple(f"aws-ipi-ovn-winc-f7-v{i}" for i in range(26))
        with self.assertRaisesRegex(wmco.SafetyError, "25"):
            dataclasses.replace(plan, selected=jobs)

    def test_ocp_minor_input_rejects_wmco_and_patch_versions(self) -> None:
        for value in ("10.21", "11.0", "4.21.1", "release-4.21", "4.021"):
            with self.subTest(value=value), self.assertRaises(wmco.SafetyError):
                wmco.validate_ocp_version(value)


class RenameAndGenerationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.plan = wmco.build_plan(RELEASE_FIXTURE, "4.99", "y")
        self.generated = (FIXTURES / "generated-ystream-periodics.yaml").read_text()

    def generated_baseline(self) -> str:
        baseline = self.generated
        for old, new in self.plan.renamed.items():
            baseline = re.sub(
                rf"(?m)^(\s+name: .*nightly-){re.escape(new)}$",
                rf"\g<1>{old}",
                baseline,
            )
            baseline = re.sub(
                rf"(?m)^(\s+- --target=){re.escape(new)}$",
                rf"\g<1>{old}",
                baseline,
            )
        return baseline

    def test_exact_generated_name_target_diff_passes(self) -> None:
        wmco.validate_generated_diff(
            self.generated_baseline(), self.generated, self.plan
        )

    def test_unrelated_same_file_generator_edit_is_rejected(self) -> None:
        with self.assertRaisesRegex(wmco.SafetyError, "beyond planned"):
            wmco.validate_generated_diff(
                self.generated_baseline(), self.generated + "# unrelated\n", self.plan
            )

    def test_untracked_generator_output_is_detected(self) -> None:
        text = (
            "1 .M N... 100644 100644 100644 abc def expected.yaml\n? unexpected.yaml\n"
        )
        paths, untracked = wmco.changed_paths_from_porcelain(text)
        self.assertEqual(paths, {"expected.yaml", "unexpected.yaml"})
        self.assertTrue(untracked)

    def test_config_validation_rejects_unrelated_change(self) -> None:
        before = self.plan.config_path.read_text()
        after = wmco.apply_config_renames(before, self.plan) + "# drift\n"
        with self.assertRaisesRegex(wmco.SafetyError, "exact planned"):
            wmco.validate_config_renames(before, after, self.plan)


class NotifierAndRequestTests(unittest.TestCase):
    def setUp(self) -> None:
        self.plan = wmco.build_plan(RELEASE_FIXTURE, "4.99", "z")
        self.body = (FIXTURES / "notifier-z.md").read_text()

    def test_exact_authoritative_notifier_identity_and_coverage(self) -> None:
        comments = [notifier_comment(self.body)]
        self.assertEqual(
            wmco.authoritative_notifier_comments(comments), tuple(comments)
        )
        self.assertEqual(
            set(
                wmco.validate_notifier_coverage(self.body, self.plan.expected_prow_jobs)
            ),
            set(self.plan.expected_prow_jobs),
        )

    def test_literal_public_pr86553_notifier_identity_is_accepted(self) -> None:
        comment = json.loads((FIXTURES / "github/notifier-pr86553.json").read_text())
        self.assertEqual(comment["user"]["login"], "openshift-merge-bot[bot]")
        self.assertEqual(comment["user"]["type"], "Bot")
        self.assertEqual(
            comment["performed_via_github_app"]["slug"], "openshift-merge-bot"
        )
        self.assertEqual(wmco.authoritative_notifier_comments([comment]), (comment,))

    def test_foreign_bot_and_wrong_app_are_rejected(self) -> None:
        foreign = notifier_comment(self.body)
        foreign["user"] = {"login": "other-bot", "type": "Bot"}
        wrong_app = notifier_comment(self.body, comment_id=2)
        wrong_app["performed_via_github_app"] = {"slug": "other-app"}
        missing_app = notifier_comment(self.body, comment_id=3)
        del missing_app["performed_via_github_app"]
        self.assertEqual(
            wmco.authoritative_notifier_comments([foreign, wrong_app, missing_app]), ()
        )

    def test_marker_embedded_or_malformed_table_is_rejected(self) -> None:
        for body in (
            "prefix [REHEARSALNOTIFIER]\nTest name | Repo | Type | Reason\n",
            self.body.replace("--- | --- | --- | ---", "--- | ---"),
        ):
            with self.subTest(body=body), self.assertRaises(wmco.SafetyError):
                wmco.parse_notifier_jobs(body)

    def test_every_trigger_capable_form_is_detected(self) -> None:
        for command in (
            "/pj-rehearse",
            "/pj-rehearse max",
            "/pj-rehearse more",
            "/pj-rehearse auto-ack",
            "/pj-rehearse periodic-ci-job another-job",
        ):
            with self.subTest(command=command):
                self.assertEqual(wmco.trigger_rehearsal_commands(command), (command,))
        for command in ("/pj-rehearse list", "/pj-rehearse skip", "/pj-rehearse abort"):
            self.assertEqual(wmco.trigger_rehearsal_commands(command), ())

    def test_request_context_rejects_foreign_author_and_extra_trigger(self) -> None:
        state = lifecycle_state(self.plan.expected_prow_jobs)
        comments = [notifier_comment(self.body), request_comment()]
        wmco.validate_request_comments(comments, state)
        foreign = [dict(comments[0]), dict(comments[1])]
        foreign[1]["user"] = {"login": "foreign", "type": "User"}
        with self.assertRaisesRegex(wmco.SafetyError, "request comment"):
            wmco.validate_request_comments(foreign, state)
        with self.assertRaisesRegex(wmco.SafetyError, "additional"):
            wmco.validate_request_comments(
                comments + [request_comment("/pj-rehearse more", comment_id=3)], state
            )


class MonitoringTests(unittest.TestCase):
    JOB = "periodic-ci-example-winc-zstream-f7"

    def evaluate(self, runs: list[dict[str, object]]) -> wmco.MonitoringDecision:
        return wmco.evaluate_check_runs(
            runs,
            [self.JOB],
            head_sha=HEAD_SHA,
            request_created_at=REQUEST_TIME,
            pr_number=PR_NUMBER,
        )

    def test_public_shape_success_may_close(self) -> None:
        decision = self.evaluate([check_run(self.JOB)])
        self.assertTrue(decision.may_close)
        self.assertEqual(decision.evidence[0].url, prow_url(self.JOB))

    def test_pending_success_history_collapses_to_latest_transition(self) -> None:
        url = prow_url(self.JOB)
        statuses = [
            commit_status(
                self.JOB, "pending", created_at="2026-10-07T12:01:00Z", target_url=url
            ),
            commit_status(
                self.JOB, "success", created_at="2026-10-07T12:05:00Z", target_url=url
            ),
        ]
        normalized = wmco.normalize_commit_statuses(statuses, HEAD_SHA)
        self.assertTrue(self.evaluate(normalized).may_close)

    def test_literal_public_pr86553_status_history_may_close(self) -> None:
        statuses = json.loads((FIXTURES / "github/statuses-pr86553.json").read_text())
        self.assertTrue(all("app" not in status for status in statuses))
        self.assertTrue(
            all(
                status["creator"] == {"login": "openshift-ci[bot]", "type": "Bot"}
                for status in statuses
            )
        )
        job = statuses[0]["context"].removeprefix("ci/rehearse/")
        decision = wmco.evaluate_check_runs(
            wmco.normalize_commit_statuses(
                statuses, "adec1d9d7b967db047a1208d506885bf8575dea9"
            ),
            [job],
            head_sha="adec1d9d7b967db047a1208d506885bf8575dea9",
            request_created_at="2026-10-06T20:01:26Z",
            pr_number=86553,
        )
        self.assertTrue(decision.may_close)

    def test_null_url_precursor_is_ignored_when_build_is_pinned(self) -> None:
        statuses = [
            commit_status(
                self.JOB, "pending", created_at="2026-10-07T12:00:01Z", target_url=None
            ),
            commit_status(
                self.JOB,
                "success",
                created_at="2026-10-07T12:05:00Z",
                target_url=prow_url(self.JOB),
            ),
        ]
        self.assertTrue(
            self.evaluate(wmco.normalize_commit_statuses(statuses, HEAD_SHA)).may_close
        )

    def test_newer_null_url_pending_blocks_stale_success(self) -> None:
        statuses = [
            commit_status(
                self.JOB,
                "success",
                created_at="2026-10-07T12:05:00Z",
                target_url=prow_url(self.JOB, "100"),
            ),
            commit_status(
                self.JOB,
                "pending",
                created_at="2026-10-07T12:10:00Z",
                target_url=None,
            ),
        ]
        decision = self.evaluate(wmco.normalize_commit_statuses(statuses, HEAD_SHA))
        self.assertEqual(decision.evidence[0].state, "pending")
        self.assertFalse(decision.may_close)

    def test_unorderable_null_url_pending_blocks_stale_success(self) -> None:
        statuses = [
            commit_status(
                self.JOB,
                "success",
                created_at="2026-10-07T12:05:00Z",
                target_url=prow_url(self.JOB, "100"),
            ),
            commit_status(
                self.JOB,
                "pending",
                created_at="",
                target_url=None,
            ),
        ]
        decision = self.evaluate(wmco.normalize_commit_statuses(statuses, HEAD_SHA))
        self.assertEqual(decision.evidence[0].state, "ambiguous")
        self.assertFalse(decision.may_close)

    def test_check_and_status_same_build_are_deduplicated(self) -> None:
        status = commit_status(
            self.JOB,
            "success",
            created_at="2026-10-07T12:05:00Z",
            target_url=prow_url(self.JOB),
        )
        runs = [
            check_run(self.JOB),
            *wmco.normalize_commit_statuses([status], HEAD_SHA),
        ]
        self.assertTrue(self.evaluate(runs).may_close)

    def test_equal_timestamp_conflict_is_ambiguous_but_equivalent_tie_collapses(
        self,
    ) -> None:
        timestamp = "2026-10-07T12:05:00Z"
        successful_check = check_run(self.JOB, created_at=timestamp)
        failed_status = wmco.normalize_commit_statuses(
            [
                commit_status(
                    self.JOB,
                    "failure",
                    created_at=timestamp,
                    target_url=prow_url(self.JOB),
                )
            ],
            HEAD_SHA,
        )[0]
        for runs in (
            [successful_check, failed_status],
            [failed_status, successful_check],
        ):
            with self.subTest(order=[run["source"] for run in runs]):
                decision = self.evaluate(runs)
                self.assertEqual(decision.evidence[0].state, "ambiguous")
                self.assertFalse(decision.may_close)

        successful_status = wmco.normalize_commit_statuses(
            [
                commit_status(
                    self.JOB,
                    "success",
                    created_at=timestamp,
                    target_url=prow_url(self.JOB),
                )
            ],
            HEAD_SHA,
        )[0]
        self.assertTrue(self.evaluate([successful_check, successful_status]).may_close)

    def test_two_distinct_build_ids_are_ambiguous(self) -> None:
        decision = self.evaluate(
            [check_run(self.JOB, build="100"), check_run(self.JOB, build="101")]
        )
        self.assertEqual(decision.evidence[0].state, "ambiguous")
        self.assertFalse(decision.may_close)

    def test_pending_missing_failure_error_abort_uncertain_leave_open(self) -> None:
        cases = (
            (check_run(self.JOB, status="pending", conclusion=None), "pending"),
            (check_run(self.JOB, conclusion="failure"), "failure"),
            (check_run(self.JOB, conclusion="cancelled"), "aborted"),
            (check_run(self.JOB, conclusion="neutral"), "error"),
            (check_run(self.JOB, status="mystery", conclusion=None), "unrecognized"),
        )
        for run, expected in cases:
            with self.subTest(expected=expected):
                decision = self.evaluate([run])
                self.assertEqual(decision.evidence[0].state, expected)
                self.assertFalse(decision.may_close)
        self.assertEqual(self.evaluate([]).evidence[0].state, "missing")

    def test_foreign_publisher_is_rejected(self) -> None:
        decision = self.evaluate([check_run(self.JOB, app_slug="foreign-app")])
        self.assertEqual(decision.evidence[0].state, "ambiguous")

    def test_newer_invalid_url_blocks_older_valid_success(self) -> None:
        decision = self.evaluate(
            [
                check_run(self.JOB, build="100", created_at="2026-10-07T12:05:00Z"),
                check_run(
                    self.JOB,
                    build="101",
                    created_at="2026-10-07T12:10:00Z",
                    details_url="https://evil.test/101",
                ),
            ]
        )
        self.assertEqual(decision.evidence[0].state, "ambiguous")
        self.assertFalse(decision.may_close)

    def test_newer_foreign_publisher_blocks_older_valid_success(self) -> None:
        decision = self.evaluate(
            [
                check_run(self.JOB, build="100", created_at="2026-10-07T12:05:00Z"),
                check_run(
                    self.JOB,
                    build="101",
                    created_at="2026-10-07T12:10:00Z",
                    app_slug="foreign-app",
                ),
            ]
        )
        self.assertEqual(decision.evidence[0].state, "ambiguous")
        self.assertFalse(decision.may_close)

    def test_only_exact_https_prow_pr_log_url_is_accepted(self) -> None:
        valid = prow_url(self.JOB)
        invalid = (
            valid.replace("https://", "http://"),
            valid.replace(
                "qe-private-deck-ci.apps.ci.l2s4.p1.openshiftapps.com", "evil.test"
            ),
            valid.replace("openshift_release", "other_repo"),
            valid.replace(f"/{PR_NUMBER}/", "/999/"),
            valid.rsplit("/", 1)[0],
            f"https://prow.ci.openshift.org/?marker=rehearse-{PR_NUMBER}-{self.JOB}/1",
        )
        for url in invalid:
            with self.subTest(url=url):
                self.assertEqual(
                    self.evaluate([check_run(self.JOB, details_url=url)])
                    .evidence[0]
                    .state,
                    "ambiguous",
                )


def lifecycle_state(expected_jobs: tuple[str, ...] | list[str]) -> dict[str, object]:
    body = (FIXTURES / "notifier-z.md").read_text()
    return {
        "schema": wmco.STATE_SCHEMA,
        "phase": "monitoring",
        "title": TITLE,
        "base_ref": wmco.PR_BASE_REF,
        "version": VERSION,
        "stream": STREAM,
        "branch": "wmco-qe-test",
        "local_head_sha": HEAD_SHA,
        "pr_head_sha": HEAD_SHA,
        "pr_number": PR_NUMBER,
        "pr_url": f"https://github.com/openshift/release/pull/{PR_NUMBER}",
        "expected_prow_jobs": list(expected_jobs),
        "notifier_comment_id": 1,
        "notifier_author": wmco.NOTIFIER_LOGIN,
        "notifier_app_slug": wmco.NOTIFIER_APP_SLUG,
        "request_comment_id": 2,
        "request_created_at": REQUEST_TIME,
        "request_url": f"https://github.com/openshift/release/pull/{PR_NUMBER}#issuecomment-2",
        "request_author": ACTOR,
        "authenticated_login": ACTOR,
        "rehearsal_command": "/pj-rehearse",
        "requested_head_sha": HEAD_SHA,
        "fork_repository": FORK,
        "notifier_body": body,
    }


class RequestLifecycle(wmco.Lifecycle):
    def __init__(self, root: Path, *, response: wmco.CommandResult):
        super().__init__(root, wmco.Runner())
        self.response = response
        self.body = (FIXTURES / "notifier-z.md").read_text()
        self.comments_value = [notifier_comment(self.body)]
        self.posted = False
        self.post_calls = 0
        self.readback_comment: dict[str, object] | None = None
        self.concurrent_comments: list[dict[str, object]] = []

    def current_pr(self, state):
        return {}

    def comments(self, state):
        comments = list(self.comments_value)
        if self.posted and self.response.returncode == 0:
            comments.append(self.readback_comment or json.loads(self.response.stdout))
            comments.extend(self.concurrent_comments)
        return comments

    def gh(self, args, *, cwd=None, check=True):
        if args[:3] == ["api", "--method", "POST"]:
            self.posted = True
            self.post_calls += 1
            return self.response
        raise AssertionError(args)


class CloseLifecycle(wmco.Lifecycle):
    def __init__(self, root: Path, evidence: list[dict[str, object]]):
        super().__init__(root, wmco.Runner())
        self.evidence = evidence
        self.writes: list[list[str]] = []
        self.body = (FIXTURES / "notifier-z.md").read_text()
        self.extra_comments: list[dict[str, object]] = []
        self.closed_snapshot = pr_snapshot()
        self.closed_snapshot["state"] = "CLOSED"

    def current_pr(self, state):
        return {}

    def comments(self, state):
        return [notifier_comment(self.body), request_comment(), *self.extra_comments]

    def rehearsal_evidence(self, state):
        return list(self.evidence)

    def gh(self, args, *, cwd=None, check=True):
        if args[:3] == ["api", "--method", "PATCH"]:
            self.writes.append(list(args))
            return wmco.CommandResult("{}", "", 0)
        if args[:2] == ["pr", "view"]:
            return wmco.CommandResult(json.dumps(self.closed_snapshot), "", 0)
        raise AssertionError(args)


class CreateLifecycle(wmco.Lifecycle):
    def __init__(self, root: Path, snapshot: dict[str, object]):
        super().__init__(root, wmco.Runner())
        self.snapshot = snapshot
        self.created = False
        self.writes: list[list[str]] = []

    def find_prs(self, branch):
        return [dict(self.snapshot)] if self.created else []

    def gh(self, args, *, cwd=None, check=True):
        self.writes.append(list(args))
        if args[:2] == ["pr", "create"]:
            self.created = True
            return wmco.CommandResult(str(self.snapshot["url"]), "", 0)
        raise AssertionError(args)


class ResumeLifecycle(wmco.Lifecycle):
    def __init__(self, root: Path):
        super().__init__(root, wmco.Runner(), poll_seconds=0, monitor_timeout=0)
        self.path = root / "state.json"
        self.snapshot = pr_snapshot()
        self.prs: list[dict[str, object]] = []
        self.git_calls: list[list[str]] = []
        self.create_pr_calls = 0
        self.continue_calls = 0

    def preflight_repository(self, *, prepare_runtime=True):
        return FORK, ACTOR

    def state_path(self, version, stream, run_id):
        return self.path

    def git(self, args, *, cwd=None, check=True):
        self.git_calls.append(list(args))
        if args[:2] == ["rev-parse", "HEAD"]:
            return wmco.CommandResult(HEAD_SHA + "\n", "", 0)
        if args[:2] == ["ls-remote", wmco.FORK_REMOTE]:
            return wmco.CommandResult(f"{HEAD_SHA}\trefs/heads/wmco-qe-test\n", "", 0)
        raise AssertionError(args)

    def find_prs(self, branch):
        return list(self.prs)

    def pr_snapshot(self, state):
        return dict(self.snapshot)

    def create_pr(self, state_path, state):
        self.create_pr_calls += 1
        return self.save(
            state_path,
            state,
            phase="blocked",
            blocker="offline test boundary after reconciled push",
        )

    def continue_from_pr(self, state_path, state):
        self.continue_calls += 1
        return state


class HeadGuardLifecycle(wmco.Lifecycle):
    def __init__(self, root: Path, snapshot: dict[str, object]):
        super().__init__(root, wmco.Runner(), poll_seconds=0, monitor_timeout=1)
        self.snapshot = snapshot
        self.patch_writes: list[list[str]] = []
        self.body = (
            "[REHEARSALNOTIFIER]\n\nTest name | Repo | Type | Reason\n"
            "--- | --- | --- | ---\n"
            f"{MonitoringTests.JOB} | N/A | periodic | Periodic changed\n"
        )

    def pr_snapshot(self, state):
        return dict(self.snapshot)

    def comments(self, state):
        return [notifier_comment(self.body), request_comment()]

    def rehearsal_evidence(self, state):
        return [check_run(MonitoringTests.JOB)]

    def gh(self, args, *, cwd=None, check=True):
        if args[:3] == ["api", "--method", "PATCH"]:
            self.patch_writes.append(list(args))
            return wmco.CommandResult("{}", "", 0)
        raise AssertionError(args)


class LifecycleSafetyTests(unittest.TestCase):
    JOB = MonitoringTests.JOB

    def state(self) -> dict[str, object]:
        state = lifecycle_state((self.JOB,))
        state["notifier_body"] = (
            "[REHEARSALNOTIFIER]\n\nTest name | Repo | Type | Reason\n"
            "--- | --- | --- | ---\n"
            f"{self.JOB} | N/A | periodic | Periodic changed\n"
        )
        return state

    def notifier_for_job(self) -> dict[str, object]:
        return notifier_comment(str(self.state()["notifier_body"]))

    def test_pr_snapshot_requires_exact_origin_fork(self) -> None:
        lifecycle = wmco.Lifecycle(Path("."), wmco.Runner())
        lifecycle.validate_pr_snapshot(pr_snapshot(), self.state(), require_open=True)
        with self.assertRaisesRegex(wmco.SafetyError, "unexpected fork"):
            lifecycle.validate_pr_snapshot(
                pr_snapshot(owner="other-fork"), self.state(), require_open=True
            )
        for snapshot, message in (
            (pr_snapshot(base="release-4.99"), "base changed"),
            (pr_snapshot(title="ordinary mergeable change"), "title changed"),
            (pr_snapshot(title=TITLE + " - drifted"), "title changed"),
        ):
            with (
                self.subTest(message=message),
                self.assertRaisesRegex(wmco.SafetyError, message),
            ):
                lifecycle.validate_pr_snapshot(
                    snapshot, self.state(), require_open=True
                )

    def test_create_readback_rejects_every_base_or_title_drift(self) -> None:
        snapshots = (
            pr_snapshot(base="release-4.99"),
            pr_snapshot(title="ordinary mergeable change"),
            pr_snapshot(title=TITLE + " - drifted"),
        )
        for snapshot in snapshots:
            with (
                self.subTest(snapshot=snapshot),
                tempfile.TemporaryDirectory() as directory,
            ):
                lifecycle = CreateLifecycle(Path(directory), snapshot)
                with self.assertRaises(wmco.SafetyError):
                    lifecycle.create_pr(Path(directory) / "state.json", self.state())
                self.assertEqual(
                    sum(call[:2] == ["pr", "create"] for call in lifecycle.writes), 1
                )
                self.assertFalse(
                    any(
                        call[:3] == ["api", "--method", "PATCH"]
                        for call in lifecycle.writes
                    )
                )

    def test_create_uses_stored_base_and_full_generated_title(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            lifecycle = CreateLifecycle(Path(directory), pr_snapshot())
            result = lifecycle.create_pr(Path(directory) / "state.json", self.state())
            create = next(
                call for call in lifecycle.writes if call[:2] == ["pr", "create"]
            )
            self.assertEqual(create[create.index("--base") + 1], wmco.PR_BASE_REF)
            self.assertEqual(create[create.index("--title") + 1], TITLE)
            self.assertEqual(result["phase"], "pr_created")

    def test_invalid_stored_base_or_title_blocks_creation_before_write(self) -> None:
        mutations = (
            ("base_ref", "release-4.99"),
            ("title", "ordinary mergeable change"),
            ("title", TITLE + " - drifted"),
        )
        for field, value in mutations:
            with (
                self.subTest(field=field, value=value),
                tempfile.TemporaryDirectory() as directory,
            ):
                lifecycle = CreateLifecycle(Path(directory), pr_snapshot())
                state = self.state()
                state[field] = value
                with self.assertRaises(wmco.SafetyError):
                    lifecycle.create_pr(Path(directory) / "state.json", state)
                self.assertEqual(lifecycle.writes, [])

    def test_failed_post_remains_uncertain_and_never_adopts_foreign_comment(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            lifecycle = RequestLifecycle(
                Path(directory), response=wmco.CommandResult("", "timeout", 1)
            )
            state = self.state()
            state["phase"] = "notifier_verified"
            lifecycle.comments_value = [self.notifier_for_job()]
            with self.assertRaisesRegex(wmco.SafetyError, "uncertain"):
                lifecycle.request_rehearsals(Path(directory) / "state.json", state)
            adopted = lifecycle.adopt_rehearsal_request(
                Path(directory) / "state.json",
                state | {"pending_action": "request_rehearsals"},
            )
            self.assertEqual(adopted["phase"], "uncertain")

    def test_successful_post_pins_response_id_body_time_author_and_pr(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            comment = request_comment()
            lifecycle = RequestLifecycle(
                Path(directory), response=wmco.CommandResult(json.dumps(comment), "", 0)
            )
            lifecycle.comments_value = [self.notifier_for_job()]
            state = self.state()
            state["phase"] = "notifier_verified"
            result = lifecycle.request_rehearsals(Path(directory) / "state.json", state)
            self.assertEqual(result["request_comment_id"], 2)
            self.assertEqual(result["request_author"], ACTOR)
            self.assertEqual(result["request_created_at"], REQUEST_TIME)

    def test_post_response_urls_require_exact_literal_pr_identity(self) -> None:
        lifecycle = wmco.Lifecycle(Path("."), wmco.Runner())
        state = self.state()
        valid_urls = (
            f"https://github.com/openshift/release/pull/{PR_NUMBER}#issuecomment-2",
            f"https://github.com/openshift/release/issues/{PR_NUMBER}#issuecomment-2",
        )
        for url in valid_urls:
            comment = request_comment()
            comment["html_url"] = url
            with self.subTest(valid_url=url):
                lifecycle.validate_posted_request(comment, state, "/pj-rehearse")
        invalid_urls = (
            f"https://github.com.evil.test/openshift/release/pull/{PR_NUMBER}#issuecomment-2",
            f"https://github.com/openshift/release/pull/{PR_NUMBER}/extra#issuecomment-2",
            f"https://github.com/openshift/release/pull/{PR_NUMBER}?x=1#issuecomment-2",
            f"https://github.com/openshift/release/pull/{PR_NUMBER}#issuecomment-3",
            f"https://git\nhub.com/openshift/release/pull/{PR_NUMBER}#issuecomment-2",
            f"https://git\rhub.com/openshift/release/pull/{PR_NUMBER}#issuecomment-2",
            f"https://git\thub.com/openshift/release/pull/{PR_NUMBER}#issuecomment-2",
            f"https://github.com/openshift/release/pull/{PR_NUMBER}#issuecomment-2\n",
            f"https://github.com/openshift/release/pull/{PR_NUMBER}#issuecomment-2\r",
            f"https://github.com/openshift/release/pull/{PR_NUMBER}#issuecomment-2\t",
            f"https://[github.com/openshift/release/pull/{PR_NUMBER}#issuecomment-2",
        )
        for url in invalid_urls:
            comment = request_comment()
            comment["html_url"] = url
            with (
                self.subTest(url=url),
                self.assertRaisesRegex(wmco.SafetyError, "expected PR"),
            ):
                lifecycle.validate_posted_request(comment, state, "/pj-rehearse")
        comment = request_comment()
        comment["issue_url"] = (
            f"https://evil.test/repos/openshift/release/issues/{PR_NUMBER}"
        )
        with self.assertRaisesRegex(wmco.SafetyError, "expected PR"):
            lifecycle.validate_posted_request(comment, state, "/pj-rehearse")
        for invalid_pr in (True, "12345", -1):
            with (
                self.subTest(pr_number=invalid_pr),
                self.assertRaisesRegex(wmco.SafetyError, "numeric PR number"),
            ):
                lifecycle.validate_posted_request(
                    request_comment(), state | {"pr_number": invalid_pr}, "/pj-rehearse"
                )

    def test_malformed_post_response_or_readback_persists_uncertain_without_retry(
        self,
    ) -> None:
        malformed = request_comment()
        malformed["html_url"] = (
            f"https://[github.com/openshift/release/pull/{PR_NUMBER}#issuecomment-2"
        )
        for location in ("response", "readback"):
            with (
                self.subTest(location=location),
                tempfile.TemporaryDirectory() as directory,
            ):
                response_comment = (
                    malformed if location == "response" else request_comment()
                )
                lifecycle = RequestLifecycle(
                    Path(directory),
                    response=wmco.CommandResult(json.dumps(response_comment), "", 0),
                )
                if location == "readback":
                    lifecycle.readback_comment = malformed
                lifecycle.comments_value = [self.notifier_for_job()]
                state = self.state()
                state["phase"] = "notifier_verified"
                state_path = Path(directory) / "state.json"
                with self.assertRaisesRegex(wmco.SafetyError, "uncertain"):
                    lifecycle.request_rehearsals(state_path, state)
                persisted = wmco.load_state(state_path)
                self.assertEqual(persisted["phase"], "uncertain")
                self.assertEqual(persisted["pending_action"], "request_rehearsals")
                self.assertEqual(lifecycle.post_calls, 1)
                with self.assertRaisesRegex(wmco.SafetyError, "duplicate trigger"):
                    lifecycle.request_rehearsals(state_path, persisted)
                self.assertEqual(lifecycle.post_calls, 1)

    def test_concurrent_same_text_foreign_comment_makes_post_uncertain(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            comment = request_comment()
            lifecycle = RequestLifecycle(
                Path(directory), response=wmco.CommandResult(json.dumps(comment), "", 0)
            )
            lifecycle.comments_value = [self.notifier_for_job()]
            foreign = request_comment(comment_id=3)
            foreign["user"] = {"login": "foreign", "type": "User"}
            lifecycle.concurrent_comments = [foreign]
            state = self.state()
            state["phase"] = "notifier_verified"
            with self.assertRaisesRegex(wmco.SafetyError, "uncertain"):
                lifecycle.request_rehearsals(Path(directory) / "state.json", state)

    def test_close_refresh_blocks_pending_second_build_and_extra_request(self) -> None:
        initial = wmco.MonitoringDecision(
            "success",
            (wmco.JobEvidence(self.JOB, "success", prow_url(self.JOB), "ok"),),
        )
        cases = (
            [check_run(self.JOB, status="pending", conclusion=None)],
            [check_run(self.JOB, conclusion="failure")],
            [check_run(self.JOB, build="100"), check_run(self.JOB, build="101")],
            wmco.normalize_commit_statuses(
                [
                    commit_status(
                        self.JOB,
                        "success",
                        created_at="2026-10-07T12:05:00Z",
                        target_url=prow_url(self.JOB, "100"),
                    ),
                    commit_status(
                        self.JOB,
                        "pending",
                        created_at="2026-10-07T12:10:00Z",
                        target_url=None,
                    ),
                ],
                HEAD_SHA,
            ),
            [
                check_run(self.JOB, created_at="2026-10-07T12:05:00Z"),
                *wmco.normalize_commit_statuses(
                    [
                        commit_status(
                            self.JOB,
                            "failure",
                            created_at="2026-10-07T12:05:00Z",
                            target_url=prow_url(self.JOB),
                        )
                    ],
                    HEAD_SHA,
                ),
            ],
            [
                check_run(self.JOB, build="100", created_at="2026-10-07T12:05:00Z"),
                check_run(
                    self.JOB,
                    build="101",
                    created_at="2026-10-07T12:10:00Z",
                    details_url="https://evil.test/101",
                ),
            ],
            [
                check_run(self.JOB, build="100", created_at="2026-10-07T12:05:00Z"),
                check_run(
                    self.JOB,
                    build="101",
                    created_at="2026-10-07T12:10:00Z",
                    app_slug="foreign-app",
                ),
            ],
        )
        for evidence in cases:
            with (
                self.subTest(evidence=evidence),
                tempfile.TemporaryDirectory() as directory,
            ):
                lifecycle = CloseLifecycle(Path(directory), evidence)
                lifecycle.body = str(self.state()["notifier_body"])
                with self.assertRaisesRegex(wmco.SafetyError, "fresh pre-close"):
                    lifecycle.close_pr(
                        Path(directory) / "state.json", self.state(), initial
                    )
                self.assertEqual(lifecycle.writes, [])
        with tempfile.TemporaryDirectory() as directory:
            lifecycle = CloseLifecycle(Path(directory), [check_run(self.JOB)])
            lifecycle.body = str(self.state()["notifier_body"])
            lifecycle.extra_comments = [
                request_comment("/pj-rehearse more", comment_id=3)
            ]
            with self.assertRaisesRegex(wmco.SafetyError, "additional"):
                lifecycle.close_pr(
                    Path(directory) / "state.json", self.state(), initial
                )
            self.assertEqual(lifecycle.writes, [])

    def test_close_after_fresh_success_uses_patch_close_and_never_merge(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            lifecycle = CloseLifecycle(
                Path(directory),
                wmco.normalize_commit_statuses(
                    [
                        commit_status(
                            self.JOB,
                            "pending",
                            created_at="2026-10-07T12:01:00Z",
                            target_url=None,
                        ),
                        commit_status(
                            self.JOB,
                            "pending",
                            created_at="2026-10-07T12:02:00Z",
                            target_url=prow_url(self.JOB),
                        ),
                        commit_status(
                            self.JOB,
                            "success",
                            created_at="2026-10-07T12:05:00Z",
                            target_url=prow_url(self.JOB),
                        ),
                    ],
                    HEAD_SHA,
                ),
            )
            lifecycle.body = str(self.state()["notifier_body"])
            decision = wmco.MonitoringDecision(
                "success",
                (wmco.JobEvidence(self.JOB, "success", prow_url(self.JOB), "ok"),),
            )
            result = lifecycle.close_pr(
                Path(directory) / "state.json", self.state(), decision
            )
            self.assertEqual(result["phase"], "closed")
            self.assertEqual(len(lifecycle.writes), 1)
            self.assertIn("state=closed", lifecycle.writes[0])
            self.assertNotIn("merge", " ".join(lifecycle.writes[0]).lower())

    def resumable_state(
        self, phase: str, pending_action: str | None
    ) -> dict[str, object]:
        state = self.state()
        state.update(
            {
                "version": VERSION,
                "stream": STREAM,
                "run_id": RUN_ID_1,
                "phase": phase,
                "pending_action": pending_action,
                "worktree": "/offline/worktree",
                "title": TITLE,
            }
        )
        return state

    def test_resume_reconciles_prepared_push_before_dispatching_forward(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            lifecycle = ResumeLifecycle(Path(directory))
            state = self.resumable_state("prepared", "push")
            wmco.atomic_write_json(lifecycle.path, state)
            _, result = lifecycle.resume("4.99", "z", run_id=RUN_ID_1)
            self.assertEqual(result["phase"], "blocked")
            self.assertIn(
                ["ls-remote", wmco.FORK_REMOTE, "refs/heads/wmco-qe-test"],
                lifecycle.git_calls,
            )
            self.assertFalse(
                any(call and call[0] == "push" for call in lifecycle.git_calls)
            )
            self.assertEqual(lifecycle.create_pr_calls, 1)

    def test_resume_adopts_exact_pr_after_uncertain_create(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            lifecycle = ResumeLifecycle(Path(directory))
            lifecycle.prs = [pr_snapshot()]
            state = self.resumable_state("pushed", "create_pr")
            wmco.atomic_write_json(lifecycle.path, state)
            _, result = lifecycle.resume("4.99", "z", run_id=RUN_ID_1)
            self.assertEqual(result["phase"], "pr_created")
            self.assertEqual(result["pr_number"], PR_NUMBER)
            self.assertEqual(lifecycle.continue_calls, 1)
            self.assertEqual(lifecycle.create_pr_calls, 0)

    def test_resume_never_adopts_pr_with_base_or_title_drift(self) -> None:
        for snapshot in (
            pr_snapshot(base="release-4.99"),
            pr_snapshot(title="ordinary mergeable change"),
            pr_snapshot(title=TITLE + " - drifted"),
        ):
            with (
                self.subTest(snapshot=snapshot),
                tempfile.TemporaryDirectory() as directory,
            ):
                lifecycle = ResumeLifecycle(Path(directory))
                lifecycle.prs = [snapshot]
                state = self.resumable_state("pushed", "create_pr")
                wmco.atomic_write_json(lifecycle.path, state)
                with self.assertRaises(wmco.SafetyError):
                    lifecycle.resume("4.99", "z", run_id=RUN_ID_1)
                self.assertEqual(lifecycle.continue_calls, 0)

    def test_resume_never_adopts_uncertain_rehearsal_post(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            lifecycle = ResumeLifecycle(Path(directory))
            state = self.resumable_state("notifier_verified", "request_rehearsals")
            wmco.atomic_write_json(lifecycle.path, state)
            _, result = lifecycle.resume("4.99", "z", run_id=RUN_ID_1)
            self.assertEqual(result["phase"], "uncertain")
            self.assertIn("cannot safely adopt", result["blocker"])
            self.assertEqual(lifecycle.continue_calls, 0)

    def test_resume_reconciles_pending_close_only_when_closed_and_unmerged(
        self,
    ) -> None:
        cases = (
            ("CLOSED", None, "closed"),
            ("OPEN", None, "uncertain"),
            ("CLOSED", "2026-10-07T12:30:00Z", "uncertain"),
        )
        for pr_state, merged_at, expected in cases:
            with (
                self.subTest(pr_state=pr_state, merged_at=merged_at),
                tempfile.TemporaryDirectory() as directory,
            ):
                lifecycle = ResumeLifecycle(Path(directory))
                lifecycle.snapshot["state"] = pr_state
                lifecycle.snapshot["mergedAt"] = merged_at
                state = self.resumable_state("monitoring", "close_without_merge")
                wmco.atomic_write_json(lifecycle.path, state)
                _, result = lifecycle.resume("4.99", "z", run_id=RUN_ID_1)
                self.assertEqual(result["phase"], expected)
                self.assertEqual(lifecycle.create_pr_calls, 0)

    def test_resume_close_reconciliation_rejects_base_and_title_drift(self) -> None:
        for field, value in (
            ("baseRefName", "release-4.99"),
            ("title", "ordinary mergeable change"),
            ("title", TITLE + " - drifted"),
        ):
            with (
                self.subTest(field=field),
                tempfile.TemporaryDirectory() as directory,
            ):
                lifecycle = ResumeLifecycle(Path(directory))
                lifecycle.snapshot["state"] = "CLOSED"
                lifecycle.snapshot[field] = value
                state = self.resumable_state("monitoring", "close_without_merge")
                wmco.atomic_write_json(lifecycle.path, state)
                _, result = lifecycle.resume("4.99", "z", run_id=RUN_ID_1)
                self.assertEqual(result["phase"], "uncertain")
                self.assertIn("snapshot is unsafe", result["blocker"])

    def test_stale_pr_head_blocks_monitor_and_close_without_patch(self) -> None:
        decision = wmco.MonitoringDecision(
            "success",
            (wmco.JobEvidence(self.JOB, "success", prow_url(self.JOB), "ok"),),
        )
        for operation in ("monitor", "close"):
            with (
                self.subTest(operation=operation),
                tempfile.TemporaryDirectory() as directory,
            ):
                lifecycle = HeadGuardLifecycle(
                    Path(directory), pr_snapshot(sha="b" * 40)
                )
                state = self.state()
                with self.assertRaisesRegex(wmco.SafetyError, "head changed"):
                    if operation == "monitor":
                        lifecycle.monitor(Path(directory) / "state.json", state)
                    else:
                        lifecycle.close_pr(
                            Path(directory) / "state.json", state, decision
                        )
                self.assertEqual(lifecycle.patch_writes, [])

    def test_pr_base_and_title_drift_block_monitor_and_close_without_patch(
        self,
    ) -> None:
        decision = wmco.MonitoringDecision(
            "success",
            (wmco.JobEvidence(self.JOB, "success", prow_url(self.JOB), "ok"),),
        )
        for snapshot in (
            pr_snapshot(base="release-4.99"),
            pr_snapshot(title="ordinary mergeable change"),
            pr_snapshot(title=TITLE + " - drifted"),
        ):
            for operation in ("monitor", "close"):
                with (
                    self.subTest(snapshot=snapshot, operation=operation),
                    tempfile.TemporaryDirectory() as directory,
                ):
                    lifecycle = HeadGuardLifecycle(Path(directory), snapshot)
                    with self.assertRaises(wmco.SafetyError):
                        if operation == "monitor":
                            lifecycle.monitor(
                                Path(directory) / "state.json", self.state()
                            )
                        else:
                            lifecycle.close_pr(
                                Path(directory) / "state.json", self.state(), decision
                            )
                    self.assertEqual(lifecycle.patch_writes, [])

    def test_post_close_readback_rejects_identity_drift_without_second_patch(
        self,
    ) -> None:
        decision = wmco.MonitoringDecision(
            "success",
            (wmco.JobEvidence(self.JOB, "success", prow_url(self.JOB), "ok"),),
        )
        snapshots = (
            pr_snapshot(base="release-4.99"),
            pr_snapshot(title="ordinary mergeable change"),
            pr_snapshot(title=TITLE + " - drifted"),
        )
        for snapshot in snapshots:
            with (
                self.subTest(snapshot=snapshot),
                tempfile.TemporaryDirectory() as directory,
            ):
                lifecycle = CloseLifecycle(Path(directory), [check_run(self.JOB)])
                lifecycle.body = str(self.state()["notifier_body"])
                lifecycle.closed_snapshot = snapshot
                lifecycle.closed_snapshot["state"] = "CLOSED"
                with self.assertRaises(wmco.SafetyError):
                    lifecycle.close_pr(
                        Path(directory) / "state.json", self.state(), decision
                    )
                self.assertEqual(len(lifecycle.writes), 1)

    def test_requested_head_mismatch_blocks_monitor_and_close_without_patch(
        self,
    ) -> None:
        decision = wmco.MonitoringDecision(
            "success",
            (wmco.JobEvidence(self.JOB, "success", prow_url(self.JOB), "ok"),),
        )
        for operation in ("monitor", "close"):
            with (
                self.subTest(operation=operation),
                tempfile.TemporaryDirectory() as directory,
            ):
                lifecycle = HeadGuardLifecycle(Path(directory), pr_snapshot())
                state = self.state()
                state["requested_head_sha"] = "b" * 40
                with self.assertRaisesRegex(wmco.SafetyError, "not pinned"):
                    if operation == "monitor":
                        lifecycle.monitor(Path(directory) / "state.json", state)
                    else:
                        lifecycle.close_pr(
                            Path(directory) / "state.json", state, decision
                        )
                self.assertEqual(lifecycle.patch_writes, [])


class RecordingRunner(wmco.Runner):
    def __init__(self, dry_run: str, state_path: Path | None = None):
        self.calls: list[list[str]] = []
        self.dry_run = dry_run
        self.state_path = state_path
        self.state_existed_during_pull: list[bool] = []

    def run(self, args, *, cwd, check=True, input_text=None):
        self.calls.append(list(args))
        if list(args[:3]) == ["make", "-n", "update"]:
            return wmco.CommandResult(self.dry_run, "", 0)
        if len(args) >= 2 and args[1] == "pull" and self.state_path is not None:
            self.state_existed_during_pull.append(self.state_path.exists())
        return wmco.CommandResult("", "", 0)


class StartBoundaryLifecycle(wmco.Lifecycle):
    def __init__(
        self,
        root: Path,
        plan: wmco.JobPlan,
        runner: wmco.Runner | None = None,
        *,
        run_runtime_preflight: bool = False,
    ):
        super().__init__(root, runner or wmco.Runner())
        self.fixture_plan = plan
        self.events: list[str] = []
        self.state_file = root / "state.json"
        self.run_runtime_preflight = run_runtime_preflight

    def plan(self, version, stream, **kwargs):
        self.events.append("plan")
        return self.fixture_plan

    def preflight_repository(self, *, prepare_runtime=True):
        self.events.append("preflight")
        if self.run_runtime_preflight:
            wmco.Lifecycle.preflight_runtime(self)
        return FORK, ACTOR

    def preflight_runtime(self):
        self.events.append("runtime-preflight")

    def git(self, args, *, cwd=None, check=True):
        if args[:2] == ["status", "--porcelain=v1"]:
            return wmco.CommandResult("", "", 0)
        if args[:2] == ["fetch", wmco.UPSTREAM_REMOTE]:
            self.events.append("fetch")
            return wmco.CommandResult("", "", 0)
        if args[:2] == ["rev-parse", f"{wmco.UPSTREAM_REMOTE}/main"]:
            return wmco.CommandResult(HEAD_SHA + "\n", "", 0)
        raise AssertionError(args)

    def state_path(self, version, stream, run_id):
        return self.state_file

    def branch_name(self, version, stream, run_id):
        return f"branch-{run_id}"

    def worktree_path(self, version, stream, run_id):
        return self.release_repo / f"worktree-{run_id}"

    def assert_no_duplicate(self, branch, state_path, fork_repository):
        self.events.append("duplicate-check")

    def initialize_worktree(self, state_path, state):
        self.events.append("initialize-worktree")
        if not state_path.is_file():
            raise AssertionError("state must exist before worktree creation")
        raise wmco.SafetyError("simulated worktree failure")


class OverLimitStartLifecycle(wmco.Lifecycle):
    def __init__(self, root: Path):
        super().__init__(root, wmco.Runner())
        self.preflight_called = False

    def plan(self, version, stream, **kwargs):
        plan = wmco.build_plan(RELEASE_FIXTURE, "4.99", "z", base_sha=HEAD_SHA)
        jobs = tuple(f"aws-ipi-ovn-winc-f7-v{i}" for i in range(26))
        return dataclasses.replace(plan, selected=jobs)

    def preflight_repository(self, *, prepare_runtime=True):
        self.preflight_called = True
        return FORK, ACTOR


class LifecyclePreparationTests(unittest.TestCase):
    def test_public_prow_publisher_identity_is_exact_and_not_configurable(self) -> None:
        self.assertEqual(wmco.AUTHORITATIVE_PROW_ACCOUNT, "openshift-ci[bot]")

    def test_run_ids_are_canonical_and_idempotent_for_zero_padded_years(
        self,
    ) -> None:
        current_year = wmco.dt.datetime.now(wmco.dt.timezone.utc).year
        run_ids = (
            "00010101t000000z-00000001",
            "09991231t235959z-ffffffff",
            f"{current_year:04d}1007t120000z-00000001",
        )
        for run_id in run_ids:
            with self.subTest(run_id=run_id):
                canonical = wmco.validate_run_id(run_id)
                self.assertEqual(canonical, run_id)
                self.assertIsNotNone(wmco.RUN_ID_RE.fullmatch(canonical))

    def test_lifecycle_paths_revalidate_all_untrusted_components(self) -> None:
        lifecycle = wmco.Lifecycle(Path("."), wmco.Runner())
        for method in (
            lifecycle.state_path,
            lifecycle.branch_name,
            lifecycle.worktree_path,
        ):
            for args in (
                ("4.21/../../tmp", "z", RUN_ID_1),
                ("4.21", "../../tmp", RUN_ID_1),
                ("4.21", "z", "../../tmp"),
            ):
                with (
                    self.subTest(method=method.__name__, args=args),
                    self.assertRaises(wmco.SafetyError),
                ):
                    method(*args)

    def test_lifecycle_path_symlinks_cannot_escape_intended_roots(self) -> None:
        with (
            tempfile.TemporaryDirectory() as directory,
            tempfile.TemporaryDirectory() as outside_directory,
        ):
            parent = Path(directory)
            repo = parent / "release"
            common_dir = repo / ".git"
            common_dir.mkdir(parents=True)
            outside = Path(outside_directory)
            (common_dir / "wmco-qe-trigger").symlink_to(
                outside / "state", target_is_directory=True
            )
            (parent / ".wmco-qe-worktrees").symlink_to(
                outside / "worktrees", target_is_directory=True
            )
            lifecycle = wmco.Lifecycle(repo, wmco.Runner())
            with (
                mock.patch.object(
                    lifecycle,
                    "git",
                    return_value=wmco.CommandResult(str(common_dir), "", 0),
                ),
                self.assertRaisesRegex(wmco.SafetyError, "escapes"),
            ):
                lifecycle.state_path("4.21", "z", RUN_ID_1)
            with self.assertRaisesRegex(wmco.SafetyError, "escapes"):
                lifecycle.worktree_path("4.21", "z", RUN_ID_1)

    def test_https_and_ssh_origin_urls_resolve_to_exact_fork(self) -> None:
        remotes = (
            "https://github.com/qe-bot/release.git",
            "git@github.com:qe-bot/release.git",
            "ssh://git@github.com/qe-bot/release.git",
            "ssh://git@github.com:22/qe-bot/release.git",
            "http://localhost:8080/github.com/qe-bot/release.git",
        )
        for remote in remotes:
            with self.subTest(remote=remote):
                self.assertEqual(wmco.redact_remote(remote), FORK)

    def test_ambiguous_or_foreign_remote_urls_are_rejected(self) -> None:
        remotes = (
            "https://evil.example/github.com/openshift/release",
            "ssh://evil.example/tmp/github.com:qe-bot/release.git",
            "https://user@github.com/qe-bot/release.git",
            "https://user:secret@github.com/qe-bot/release.git",
            "https://github.com/qe-bot/release.git?ref=main",
            "https://github.com/qe-bot/release.git#main",
            "https://github.com/prefix/qe-bot/release.git",
            "https://github.com/qe-bot/release/extra",
            "https://github.com.evil.example/qe-bot/release.git",
            "git@evil.example:qe-bot/release.git",
            "git@github.com:prefix/qe-bot/release.git",
            "ssh://root@github.com/qe-bot/release.git",
            "http://localhost:8080/tmp/github.com/qe-bot/release.git",
            "http://localhost.evil.example:8080/github.com/qe-bot/release.git",
        )
        for remote in remotes:
            with self.subTest(remote=remote):
                self.assertEqual(wmco.redact_remote(remote), "unrecognized Git remote")

    def test_remote_bypasses_stop_preflight_before_fetch_or_push(self) -> None:
        class PreflightRunner(wmco.Runner):
            def __init__(self, upstream: str, origin: str) -> None:
                self.upstream = upstream
                self.origin = origin
                self.calls: list[list[str]] = []

            def run(self, args, *, cwd, check=True, input_text=None):
                command = list(args)
                self.calls.append(command)
                if command == ["git", "rev-parse", "--is-inside-work-tree"]:
                    return wmco.CommandResult("true\n", "", 0)
                if command == ["git", "remote", "get-url", wmco.UPSTREAM_REMOTE]:
                    return wmco.CommandResult(self.upstream + "\n", "", 0)
                if command == ["gh", "auth", "status"]:
                    return wmco.CommandResult("", "", 0)
                if command == ["git", "remote", "get-url", wmco.FORK_REMOTE]:
                    return wmco.CommandResult(self.origin + "\n", "", 0)
                raise AssertionError(command)

        cases = (
            (
                "https://evil.example/github.com/openshift/release",
                "https://github.com/qe-bot/release.git",
            ),
            (
                "https://github.com/openshift/release.git",
                "ssh://evil.example/tmp/github.com:qe-bot/release.git",
            ),
        )
        for upstream, origin in cases:
            runner = PreflightRunner(upstream, origin)
            lifecycle = wmco.Lifecycle(Path("."), runner)
            with (
                self.subTest(upstream=upstream, origin=origin),
                self.assertRaises(wmco.SafetyError),
            ):
                lifecycle.preflight_repository(prepare_runtime=False)
            mutating_git_verbs = {"fetch", "push"}
            self.assertFalse(
                any(
                    len(command) > 1
                    and command[0] == "git"
                    and command[1] in mutating_git_verbs
                    for command in runner.calls
                )
            )

    def test_26_job_start_stops_before_preflight_or_writes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            lifecycle = OverLimitStartLifecycle(Path(directory))
            with self.assertRaisesRegex(wmco.SafetyError, "25"):
                lifecycle.start(
                    "4.99",
                    "z",
                    approved_plan_digest="0" * 64,
                    run_id=RUN_ID_1,
                )
            self.assertFalse(lifecycle.preflight_called)
            self.assertEqual(list(Path(directory).iterdir()), [])

    def test_preflight_checks_engine_pyyaml_generator_pull_and_disk(self) -> None:
        runner = RecordingRunner(
            "false || podman pull --platform linux "
            "quay.io/openshift/ci-public:ci_ci-operator-checkconfig_latest\n"
            "false || podman --log-level warning pull --quiet "
            "quay.io/openshift/generator:latest\n"
        )
        lifecycle = wmco.Lifecycle(Path("."), runner, container_engine="podman")
        lifecycle.preflight_runtime()
        self.assertIn(["podman", "info"], runner.calls)
        self.assertIn([sys.executable, "-c", "import yaml"], runner.calls)
        self.assertIn(
            ["podman", "pull", "--quiet", "quay.io/openshift/generator:latest"],
            runner.calls,
        )
        self.assertIn(
            [
                "podman",
                "pull",
                "--quiet",
                "quay.io/openshift/ci-public:ci_ci-operator-checkconfig_latest",
            ],
            runner.calls,
        )

    def test_guarded_docker_pull_recipe_is_supported(self) -> None:
        runner = RecordingRunner(
            "false || docker pull --platform linux "
            "quay.io/openshift/ci-public:ci_auto-config-brancher_latest\n"
        )
        lifecycle = wmco.Lifecycle(Path("."), runner, container_engine="docker")
        lifecycle.preflight_runtime()
        self.assertIn(
            [
                "docker",
                "pull",
                "--quiet",
                "quay.io/openshift/ci-public:ci_auto-config-brancher_latest",
            ],
            runner.calls,
        )

    def test_generator_images_are_prepulled_before_state_and_worktree(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            plan = wmco.build_plan(RELEASE_FIXTURE, "4.99", "z", base_sha=HEAD_SHA)
            state_path = root / "state.json"
            runner = RecordingRunner(
                "false || podman pull --platform linux "
                "quay.io/openshift/generator:latest\n",
                state_path,
            )
            lifecycle = StartBoundaryLifecycle(
                root, plan, runner, run_runtime_preflight=True
            )
            with self.assertRaisesRegex(wmco.SafetyError, "worktree failure"):
                lifecycle.start(
                    "4.99",
                    "z",
                    approved_plan_digest=plan.digest,
                    run_id=RUN_ID_1,
                )
            self.assertEqual(runner.state_existed_during_pull, [False])
            self.assertTrue(state_path.is_file())
            self.assertIn("initialize-worktree", lifecycle.events)

    def test_read_only_plan_and_state_precede_worktree_failure(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            plan = wmco.build_plan(RELEASE_FIXTURE, "4.99", "z", base_sha=HEAD_SHA)
            lifecycle = StartBoundaryLifecycle(Path(directory), plan)
            with self.assertRaisesRegex(wmco.SafetyError, "worktree failure"):
                lifecycle.start(
                    "4.99",
                    "z",
                    approved_plan_digest=plan.digest,
                    run_id=RUN_ID_1,
                )
            self.assertTrue(lifecycle.state_file.is_file())
            stored = json.loads(lifecycle.state_file.read_text())
            self.assertEqual(stored["base_ref"], wmco.PR_BASE_REF)
            self.assertEqual(stored["title"], TITLE)
            self.assertLess(
                lifecycle.events.index("plan"), lifecycle.events.index("preflight")
            )
            self.assertLess(
                lifecycle.events.index("duplicate-check"),
                lifecycle.events.index("initialize-worktree"),
            )

    def test_plan_digest_binds_base_catalog_jobs_and_command(self) -> None:
        plan = wmco.build_plan(RELEASE_FIXTURE, "4.99", "z", base_sha=HEAD_SHA)
        self.assertNotEqual(
            plan.digest, dataclasses.replace(plan, base_sha="b" * 40).digest
        )
        changed_inputs = dict(plan.safety_inputs)
        changed_inputs[next(iter(changed_inputs))] = "changed"
        self.assertNotEqual(
            plan.digest, dataclasses.replace(plan, safety_inputs=changed_inputs).digest
        )
        with tempfile.TemporaryDirectory() as directory:
            lifecycle = StartBoundaryLifecycle(Path(directory), plan)
            with self.assertRaisesRegex(wmco.SafetyError, "approved-plan-digest"):
                lifecycle.start(
                    "4.99",
                    "z",
                    approved_plan_digest="0" * 64,
                    run_id=RUN_ID_1,
                )
            self.assertEqual(lifecycle.events, ["plan"])

    def test_collision_resistant_names_allow_two_same_release_runs(self) -> None:
        lifecycle = wmco.Lifecycle(Path("."), wmco.Runner())
        self.assertNotEqual(
            lifecycle.branch_name("4.21", "z", RUN_ID_1),
            lifecycle.branch_name("4.21", "z", RUN_ID_2),
        )
        self.assertNotEqual(
            lifecycle.worktree_path("4.21", "z", RUN_ID_1),
            lifecycle.worktree_path("4.21", "z", RUN_ID_2),
        )

    def test_live_confirmation_is_required_before_any_lifecycle_call(self) -> None:
        stderr = io.StringIO()
        with redirect_stderr(stderr):
            result = wmco.main(
                [
                    "start",
                    "4.99",
                    "z-stream",
                    "--release-repo",
                    str(RELEASE_FIXTURE),
                    "--approved-plan-digest",
                    "0" * 64,
                ]
            )
        self.assertEqual(result, 2)
        self.assertIn("live mode requires", stderr.getvalue())


if __name__ == "__main__":
    unittest.main()
