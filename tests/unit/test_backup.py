import base64
import os
import sqlite3
import subprocess
import time
import uuid
import zipfile
from pathlib import Path

import pytest
from backup_writer import BackupWriter

from server.backup import crypto
from server.backup.exporter import export_local_backup
from server.backup.importer import ImportResult
from server.backup.importer import import_local_backup
from server.backup.importer import open_local_backup
from server.backup.proto import backup_pb2 as pb
from server.db import recipients as dbr
from server.db.connection import connect
from server.db.connection import init_schema
from server.db.files import FileStore
from server.signal.account_file import AccountSecrets

# Public test vector from libsignal (rust/message-backup/src/key.rs), not a real account's key.
RECOVERY_KEY = "dtjs858asj6tv0jzsqrsmj0ubp335pisj98e9ssnss8myoc08drhtcktyawvx45l"  # gitleaks:allow
SELF_RID, BOB_RID, CAROL_RID, GROUP_RID = 1, 2, 3, 4
BOB_ACI = uuid.UUID("00000000-0000-0000-0000-00000000000b")
CAROL_ACI = uuid.UUID("00000000-0000-0000-0000-00000000000c")
GROUP_MASTER_KEY = bytes.fromhex("6465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f80818283")
BOB_CHAT, GROUP_CHAT = 10, 11
NOW = int(time.time() * 1000)


def test_key_derivation_matches_libsignal_vectors() -> None:
    # rust/message-backup/src/key.rs test vectors.
    bk = crypto.backup_key(RECOVERY_KEY)
    backup_id = crypto.backup_id_from_aci(bk, bytes.fromhex("659aa5f4a28dfcc11ea1b997537a3d95"))
    hmac_key, aes_key = crypto.message_backup_keys(bk, backup_id)
    assert hmac_key.hex() == "f425e22a607c529717e1e1b29f9fe139f9d1c7e7d01e371c7753c544a3026649"
    assert aes_key.hex() == "e143f4ad5668d8bfed2f88562f0693f53bda2c0e55c9d71730f30e24695fd6df"


def test_group_id_matches_zkgroup_vector() -> None:
    # node/ts/test/ZKGroup-test.ts "has a string representation for GroupIdentifier".
    group_id = crypto.group_id_from_master_key(GROUP_MASTER_KEY)
    assert base64.b64encode(group_id).decode() == "hOJWcwVI+LoJBpsiPszBM/WZ+YJ+3HCE+JIeSnDNnkw="


def test_recovery_key_normalization() -> None:
    spaced = " ".join(RECOVERY_KEY.upper()[i : i + 4] for i in range(0, 64, 4))
    assert crypto.normalize_recovery_key(spaced) == RECOVERY_KEY
    with pytest.raises(crypto.BackupDecryptError):
        crypto.normalize_recovery_key("too short")
    # iOS shows "0" as "=" and "O" as "#" (vectors from Signal-iOS DisplayableAccountEntropyPoolTest).
    raw = "0o0ooaa" + "a" * 57
    assert crypto.normalize_recovery_key("=#=##AA" + "A" * 57) == raw
    assert crypto.normalize_recovery_key("=#0oOaA" + "a" * 57) == raw


def _frame(**kwargs: object) -> pb.Frame:
    return pb.Frame(**kwargs)  # type: ignore[arg-type]


def _text_item(chat: int, author: int, sent: int, body: str, incoming: bool = True) -> pb.ChatItem:
    item = pb.ChatItem(chatId=chat, authorId=author, dateSent=sent)
    item.standardMessage.text.body = body
    if incoming:
        item.incoming.dateReceived = sent + 5
        item.incoming.read = True
    else:
        status = item.outgoing.sendStatus.add(recipientId=BOB_RID, timestamp=sent + 10)
        status.read.SetInParent()
    return item


def _write_backup(tmp_path: Path) -> tuple[Path, BackupWriter]:
    root = tmp_path / "upload" / "SignalBackups"
    w = BackupWriter(root, RECOVERY_KEY)
    w.add(_frame(account=pb.AccountData(givenName="Me")))
    w.add(_frame(recipient=pb.Recipient(id=SELF_RID, self=pb.Self())))
    bob = pb.Recipient(id=BOB_RID)
    bob.contact.aci = BOB_ACI.bytes
    bob.contact.e164 = 15550000002
    bob.contact.profileGivenName = "Bob"
    bob.contact.systemGivenName = "Robert"
    bob.contact.registered.SetInParent()
    w.add(_frame(recipient=bob))
    carol = pb.Recipient(id=CAROL_RID)
    carol.contact.aci = CAROL_ACI.bytes
    carol.contact.profileGivenName = "Carol"
    w.add(_frame(recipient=carol))
    group = pb.Recipient(id=GROUP_RID)
    group.group.masterKey = GROUP_MASTER_KEY
    group.group.snapshot.title.title = "Fam"
    group.group.snapshot.members.add(userId=BOB_ACI.bytes, role=pb.Group.Member.ADMINISTRATOR)
    group.group.snapshot.members.add(userId=CAROL_ACI.bytes, role=pb.Group.Member.DEFAULT)
    w.add(_frame(recipient=group))
    w.add(_frame(chat=pb.Chat(id=BOB_CHAT, recipientId=BOB_RID, archived=False)))
    w.add(_frame(chat=pb.Chat(id=GROUP_CHAT, recipientId=GROUP_RID, pinnedOrder=1)))

    w.add(_frame(chatItem=_text_item(BOB_CHAT, BOB_RID, 1000, "hello from bob")))
    reply = _text_item(BOB_CHAT, SELF_RID, 2000, "hi bob", incoming=False)
    reply.standardMessage.quote.targetSentTimestamp = 1000
    reply.standardMessage.quote.authorId = BOB_RID
    reply.standardMessage.quote.text.body = "hello from bob"
    reply.standardMessage.reactions.add(emoji="👍", authorId=BOB_RID, sentTimestamp=2100)
    w.add(_frame(chatItem=reply))

    edited = _text_item(BOB_CHAT, BOB_RID, 3000, "second version")
    edited.revisions.append(_text_item(BOB_CHAT, BOB_RID, 2900, "first version"))
    edited.standardMessage.text.bodyRanges.add(start=0, length=6, style=pb.BodyRange.BOLD)
    w.add(_frame(chatItem=edited))

    photo = _text_item(GROUP_CHAT, CAROL_RID, 4000, "look")
    att = photo.standardMessage.attachments.add()
    att.pointer.CopyFrom(w.attachment(b"\x89PNG fake image bytes", "image/png", "cat.png"))
    missing = photo.standardMessage.attachments.add()
    missing.pointer.contentType = "video/mp4"  # no localKey: wasn't on the phone
    w.add(_frame(chatItem=photo))

    deleted = pb.ChatItem(chatId=GROUP_CHAT, authorId=BOB_RID, dateSent=5000)
    deleted.incoming.dateReceived = 5001
    deleted.remoteDeletedMessage.SetInParent()
    w.add(_frame(chatItem=deleted))

    expired = _text_item(BOB_CHAT, BOB_RID, 6000, "gone already")
    expired.expiresInMs = 60_000
    expired.expireStartDate = 6000
    w.add(_frame(chatItem=expired))

    pending_expiry = _text_item(BOB_CHAT, BOB_RID, NOW, "disappears later")
    pending_expiry.expiresInMs = 3_600_000
    pending_expiry.expireStartDate = NOW
    w.add(_frame(chatItem=pending_expiry))

    call = pb.ChatItem(chatId=BOB_CHAT, authorId=BOB_RID, dateSent=7000)
    call.directionless.SetInParent()
    call.updateMessage.individualCall.direction = pb.IndividualCall.INCOMING
    call.updateMessage.individualCall.type = pb.IndividualCall.VIDEO_CALL
    call.updateMessage.individualCall.state = pb.IndividualCall.MISSED
    w.add(_frame(chatItem=call))
    w.write()
    return tmp_path / "upload", w


def _zip(folder: Path) -> Path:
    """Zip a folder the way a user would before uploading it: the folder itself at the top of the archive."""
    zip_path = folder.with_suffix(".zip")
    with zipfile.ZipFile(zip_path, "w") as zf:
        for path in sorted(folder.rglob("*")):
            zf.write(path, path.relative_to(folder.parent).as_posix())
    return zip_path


def _import(
    upload: Path, conn: sqlite3.Connection, files: FileStore, recovery_key: str = RECOVERY_KEY
) -> ImportResult:
    with open_local_backup(_zip(upload)) as backup:
        return import_local_backup(backup, recovery_key, conn, files, lambda *_: None)


def test_import_local_backup(tmp_path: Path, conn: sqlite3.Connection, files: FileStore) -> None:
    upload, _ = _write_backup(tmp_path)
    result = _import(upload, conn, files)
    assert result.messages_imported == 7  # all but the already-expired one
    assert result.attachments_imported == 1

    bob = conn.execute("SELECT * FROM recipients WHERE aci = ?", (str(BOB_ACI),)).fetchone()
    assert (bob["e164"], bob["profile_name"], bob["contact_name"]) == ("+15550000002", "Bob", "Robert")

    group = conn.execute("SELECT * FROM recipients WHERE kind = 'group'").fetchone()
    assert group["group_id"] == "hOJWcwVI+LoJBpsiPszBM/WZ+YJ+3HCE+JIeSnDNnkw="
    assert group["group_title"] == "Fam"
    roles = dict(
        conn.execute(
            "SELECT r.aci, gm.role FROM group_members gm JOIN recipients r ON r.id = gm.member_recipient_id"
        ).fetchall()
    )
    assert roles == {str(BOB_ACI): "admin", str(CAROL_ACI): "member"}

    by_sent = {m["sent_at"]: m for m in conn.execute("SELECT * FROM messages")}
    assert 6000 not in by_sent
    assert by_sent[2000]["author_id"] == dbr.self_recipient_id(conn)
    assert by_sent[2000]["quote_sent_at"] == 1000
    assert (
        conn.execute("SELECT status FROM receipts WHERE message_id = ?", (by_sent[2000]["id"],)).fetchone()[0]
        == "read"
    )
    assert (
        conn.execute("SELECT emoji FROM reactions WHERE message_id = ?", (by_sent[2000]["id"],)).fetchone()[0] == "👍"
    )

    # Edited messages are keyed by the original's timestamp, which is what live edits/reactions/quotes target.
    edited = by_sent[2900]
    assert (edited["body"], edited["edited_at"]) == ("second version", 3000)
    assert '"BOLD"' in edited["body_ranges"]
    revisions = conn.execute("SELECT body, edited_at FROM message_edits WHERE message_id = ?", (edited["id"],))
    assert [tuple(r) for r in revisions] == [("first version", 2900)]

    present, missing = conn.execute(
        "SELECT * FROM attachments WHERE message_id = ? ORDER BY ordinal", (by_sent[4000]["id"],)
    ).fetchall()
    assert present["status"] == "present"
    assert files.path_for(present["file_sha256"]).read_bytes() == b"\x89PNG fake image bytes"
    assert missing["status"] == "missing"

    assert by_sent[5000]["deleted_at"] is not None
    assert by_sent[NOW]["expires_at"] == NOW + 3_600_000
    assert by_sent[7000]["kind"] == "update"
    assert '"missed": true' in by_sent[7000]["update_payload"]

    pinned = conn.execute("SELECT pinned FROM conversations WHERE recipient_id = ?", (group["id"],)).fetchone()[0]
    assert pinned == 1


def test_reimport_is_idempotent(tmp_path: Path, conn: sqlite3.Connection, files: FileStore) -> None:
    upload, _ = _write_backup(tmp_path)
    _import(upload, conn, files)
    counts = conn.execute("SELECT (SELECT count(*) FROM messages), (SELECT count(*) FROM attachments)").fetchone()
    stored_files = sorted(p.name for p in files.path_for("00").parent.parent.rglob("*") if p.is_file())

    result = _import(upload, conn, files)
    assert result.messages_imported == 0
    assert result.messages_skipped == 7
    assert (
        conn.execute("SELECT (SELECT count(*) FROM messages), (SELECT count(*) FROM attachments)").fetchone() == counts
    )
    assert sorted(p.name for p in files.path_for("00").parent.parent.rglob("*") if p.is_file()) == stored_files


def test_wrong_recovery_key_is_rejected(tmp_path: Path, conn: sqlite3.Connection, files: FileStore) -> None:
    upload, _ = _write_backup(tmp_path)
    with pytest.raises(crypto.BackupDecryptError, match="recovery key doesn't match"):
        _import(upload, conn, files, recovery_key="a" * 64)


def test_legacy_backup_gets_a_clear_error(tmp_path: Path) -> None:
    (tmp_path / "upload").mkdir()
    (tmp_path / "upload" / "signal-2026-01-01-00-00-00.backup").write_bytes(b"x")
    with (
        pytest.raises(crypto.BackupDecryptError, match="legacy Android"),
        open_local_backup(_zip(tmp_path / "upload")),
    ):
        pass


def test_not_a_zip_gets_a_clear_error(tmp_path: Path) -> None:
    (tmp_path / "upload.zip").write_bytes(b"not a zip")
    with (
        pytest.raises(crypto.BackupDecryptError, match="isn't a valid zip"),
        open_local_backup(tmp_path / "upload.zip"),
    ):
        pass


def test_finds_backup_zipped_at_any_level(tmp_path: Path, conn: sqlite3.Connection, files: FileStore) -> None:
    """Users may zip the SignalBackups folder, its contents, or just the snapshot folder plus files/."""
    upload, writer = _write_backup(tmp_path)
    contents = tmp_path / "contents.zip"
    with zipfile.ZipFile(contents, "w") as zf:
        for path in sorted(writer.root.rglob("*")):
            zf.write(path, path.relative_to(writer.root).as_posix())
        zf.writestr("__MACOSX/._junk", b"")
    with open_local_backup(contents) as backup:
        assert backup.snapshot == "signal-backup-2026-10-01-12-00-00/"
        assert backup.files_prefix == "files/"
        assert import_local_backup(backup, RECOVERY_KEY, conn, files, lambda *_: None).attachments_imported == 1


def _snapshot(conn: sqlite3.Connection, files: FileStore) -> dict[str, object]:
    """A comparable view of the database, independent of row ids."""

    def who(recipient_id: int | None) -> str | None:
        if recipient_id is None:
            return None
        r = conn.execute("SELECT kind, aci, e164, group_id FROM recipients WHERE id = ?", (recipient_id,)).fetchone()
        return "self" if r["kind"] == "self" else (r["aci"] or r["e164"] or r["group_id"])

    messages = []
    for m in conn.execute(
        "SELECT m.*, c.recipient_id AS chat FROM messages m JOIN conversations c ON c.id = m.conversation_id"
    ):
        messages.append(
            (
                who(m["chat"]),
                who(m["author_id"]),
                m["sent_at"],
                m["kind"],
                m["body"],
                m["body_ranges"],
                m["edited_at"],
                m["deleted_at"] is not None,
                m["expires_at"],
                who(m["quote_author_id"]),
                m["quote_sent_at"],
                m["update_payload"],
                tuple(
                    (
                        a["content_type"],
                        a["filename"],
                        a["status"],
                        files.path_for(a["file_sha256"]).read_bytes() if a["file_sha256"] else None,
                    )
                    for a in conn.execute(
                        "SELECT * FROM attachments WHERE message_id = ? ORDER BY ordinal", (m["id"],)
                    )
                ),
                tuple(
                    sorted(
                        (who(r["reactor_id"]), r["emoji"])
                        for r in conn.execute("SELECT * FROM reactions WHERE message_id = ?", (m["id"],))
                    )
                ),
                tuple(
                    sorted(
                        (who(r["recipient_id"]), r["status"])
                        for r in conn.execute("SELECT * FROM receipts WHERE message_id = ?", (m["id"],))
                    )
                ),
                tuple(
                    (e["body"], e["edited_at"])
                    for e in conn.execute(
                        "SELECT * FROM message_edits WHERE message_id = ? ORDER BY revision", (m["id"],)
                    )
                ),
            )
        )
    recipients = sorted(
        (r["kind"], r["aci"], r["e164"], r["group_id"], r["profile_name"], r["contact_name"], r["group_title"])
        for r in conn.execute("SELECT * FROM recipients WHERE kind != 'self'")
    )
    return {"messages": sorted(messages, key=repr), "recipients": recipients}


def test_export_round_trips_through_import(tmp_path: Path, conn: sqlite3.Connection, files: FileStore) -> None:
    upload, _ = _write_backup(tmp_path)
    _import(upload, conn, files)
    dbr.set_self_ids(conn, dbr.ContactIds(aci="00000000-0000-0000-0000-00000000000a", e164="+15550000001"))
    secrets = AccountSecrets(
        aci="00000000-0000-0000-0000-00000000000a",
        recovery_key=RECOVERY_KEY,
        profile_key=bytes(32),
        media_root_backup_key=None,
    )
    result = export_local_backup(conn, files, secrets, tmp_path / "export", lambda *_: None)
    # The message disappearing within the hour is left out: backups may not carry ones with under a day left.
    assert result.messages_skipped == 1
    assert result.attachments_exported == 1

    fresh = connect(tmp_path / "fresh.db")
    init_schema(fresh)
    fresh_files_root = tmp_path / "fresh-files"
    fresh_files_root.mkdir()
    fresh_files = FileStore(fresh_files_root)
    reimported = _import(tmp_path / "export", fresh, fresh_files)
    assert reimported.messages_imported == result.messages_exported
    conn.execute("DELETE FROM messages WHERE sent_at = ?", (NOW,))
    assert _snapshot(fresh, fresh_files) == _snapshot(conn, files)


@pytest.mark.skipif(
    not os.environ.get("SIGNAL_BACKUP_VALIDATOR"),
    reason="set SIGNAL_BACKUP_VALIDATOR to libsignal's message-backup validator binary to run",
)
def test_export_passes_libsignal_validator(tmp_path: Path, conn: sqlite3.Connection, files: FileStore) -> None:
    """libsignal's validator applies the rules the Signal apps rely on; run it against an export with edits, deletes,
    quotes, reactions, receipts, calls, attachments and groups."""
    upload, _ = _write_backup(tmp_path)
    _import(upload, conn, files)
    aci = "00000000-0000-0000-0000-00000000000a"
    dbr.set_self_ids(conn, dbr.ContactIds(aci=aci, e164="+15550000001"))
    secrets = AccountSecrets(aci=aci, recovery_key=RECOVERY_KEY, profile_key=bytes(32), media_root_backup_key=None)
    export_local_backup(conn, files, secrets, tmp_path / "export", lambda *_: None)
    [snapshot] = (tmp_path / "export" / "SignalBackups").glob("signal-backup-*")
    validator = os.environ["SIGNAL_BACKUP_VALIDATOR"]
    for key_args in (["--metadata", str(snapshot / "metadata")], ["--aci", aci]):
        result = subprocess.run(
            [validator, "--account-entropy", RECOVERY_KEY, *key_args, str(snapshot / "main")],
            capture_output=True,
            text=True,
        )
        assert result.returncode == 0, result.stderr[-3000:]
