"""Decryption for Signal's local ("on-device") backups, the format shared by iOS, Android and Desktop since 8.30.

Constants and layouts follow libsignal (rust/account-keys/src/backup.rs, rust/message-backup/src/key.rs and frame.rs)
and the apps' local-backup code; the tests check them against libsignal's published key-derivation vectors.
"""

import hashlib
import hmac
import zlib
from collections.abc import Callable
from collections.abc import Iterator
from pathlib import Path
from typing import IO

import attr
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives import padding
from cryptography.hazmat.primitives.ciphers import Cipher
from cryptography.hazmat.primitives.ciphers import algorithms
from cryptography.hazmat.primitives.ciphers import modes
from cryptography.hazmat.primitives.kdf.hkdf import HKDF

_CHUNK = 1 << 20
_REMOTE_BACKUP_MAGIC = b"SBACKUP\x01"


class BackupDecryptError(Exception):
    pass


class WrongRecoveryKey(BackupDecryptError):
    """The key is malformed or doesn't decrypt this backup; the same upload can be retried with another key."""


@attr.s(auto_attribs=True, frozen=True)
class EncryptedFile:
    """A file to decrypt, either on disk or inside an uploaded zip: its name (for errors), size, and a way to open it
    for a sequential read."""

    name: str
    size: int
    open: Callable[[], IO[bytes]]

    @staticmethod
    def from_path(path: Path) -> "EncryptedFile":
        return EncryptedFile(name=path.name, size=path.stat().st_size, open=lambda: path.open("rb"))


def _hkdf(ikm: bytes, info: bytes, length: int) -> bytes:
    return HKDF(hashes.SHA256(), length, None, info).derive(ikm)


def normalize_recovery_key(text: str) -> str:
    key = "".join(text.split()).replace("-", "").lower()
    if len(key) != 64 or not key.isalnum():
        raise WrongRecoveryKey("the recovery key should be 64 letters and digits")
    return key


def backup_key(recovery_key: str) -> bytes:
    return _hkdf(recovery_key.encode("ascii"), b"20240801_SIGNAL_BACKUP_KEY", 32)


def backup_id_from_aci(bk: bytes, aci: bytes) -> bytes:
    return _hkdf(bk, b"20241024_SIGNAL_BACKUP_ID:" + aci, 16)


def local_metadata_key(bk: bytes) -> bytes:
    return _hkdf(bk, b"20241011_SIGNAL_LOCAL_BACKUP_METADATA_KEY", 32)


def backup_id_from_metadata(bk: bytes, iv: bytes, encrypted_id: bytes) -> bytes:
    decryptor = Cipher(algorithms.AES(local_metadata_key(bk)), modes.CTR(iv + b"\0\0\0\0")).decryptor()
    return decryptor.update(encrypted_id) + decryptor.finalize()


def message_backup_keys(bk: bytes, backup_id: bytes) -> tuple[bytes, bytes]:
    """(hmac_key, aes_key) for the `main` file."""
    k = _hkdf(bk, b"20241007_SIGNAL_BACKUP_ENCRYPT_MESSAGE_BACKUP:" + backup_id, 64)
    return k[:32], k[32:]


def _verify_hmac(source: EncryptedFile, key: bytes) -> None:
    size = source.size
    if size < 16 + 16 + 32:
        raise BackupDecryptError(f"{source.name} is too short to be a backup")
    mac = hmac.new(key, digestmod=hashlib.sha256)
    with source.open() as f:
        remaining = size - 32
        while remaining:
            chunk = f.read(min(_CHUNK, remaining))
            mac.update(chunk)
            remaining -= len(chunk)
        expected = f.read(32)
    if not hmac.compare_digest(mac.digest(), expected):
        raise BackupDecryptError(f"{source.name} is corrupt or doesn't match its key")


def _decrypt_cbc_stream(
    source: EncryptedFile, aes_key: bytes, on_progress: Callable[[float], None] | None = None
) -> Iterator[bytes]:
    """Yield the PKCS7-unpadded plaintext of `IV(16) ‖ AES-256-CBC ciphertext ‖ HMAC(32)`, streaming."""
    size = source.size
    with source.open() as f:
        iv = f.read(16)
        decryptor = Cipher(algorithms.AES(aes_key), modes.CBC(iv)).decryptor()
        unpadder = padding.PKCS7(128).unpadder()
        remaining = size - 16 - 32
        while remaining:
            chunk = f.read(min(_CHUNK, remaining))
            remaining -= len(chunk)
            if on_progress is not None:
                on_progress(1 - remaining / size)
            yield unpadder.update(decryptor.update(chunk))
        yield unpadder.update(decryptor.finalize()) + unpadder.finalize()


def decrypt_main(
    source: EncryptedFile, hmac_key: bytes, aes_key: bytes, on_progress: Callable[[float], None] | None = None
) -> Iterator[bytes]:
    """Verify then stream-decrypt and gunzip a backup's `main` file, yielding decompressed chunks."""
    with source.open() as f:
        if f.read(len(_REMOTE_BACKUP_MAGIC)) == _REMOTE_BACKUP_MAGIC:
            raise BackupDecryptError("this is a Signal-hosted (cloud) backup, not an on-device backup")
    try:
        _verify_hmac(source, hmac_key)
    except BackupDecryptError as e:
        raise WrongRecoveryKey("the recovery key doesn't match this backup") from e
    inflater = zlib.decompressobj(wbits=31)
    for chunk in _decrypt_cbc_stream(source, aes_key, on_progress):
        yield inflater.decompress(chunk)
    yield inflater.flush()


def read_frames(chunks: Iterator[bytes]) -> Iterator[bytes]:
    """Split a stream into varint-length-delimited protobuf messages."""
    buf = bytearray()
    pos = 0
    for chunk in chunks:
        buf += chunk
        while True:
            length = 0
            shift = 0
            i = pos
            while i < len(buf):
                b = buf[i]
                i += 1
                length |= (b & 0x7F) << shift
                shift += 7
                if b < 0x80:
                    break
            else:
                break  # incomplete varint
            if i + length > len(buf):
                break
            yield bytes(buf[i : i + length])
            pos = i + length
        if pos > _CHUNK:
            del buf[:pos]
            pos = 0
    if pos != len(buf):
        raise BackupDecryptError("backup ends in the middle of a record")


def local_media_name(plaintext_hash: bytes, local_key: bytes) -> str:
    return hashlib.sha256(plaintext_hash + local_key).hexdigest()


def decrypt_attachment(source: EncryptedFile, local_key: bytes, size: int, plaintext_hash: bytes) -> Iterator[bytes]:
    """Stream-decrypt a local-backup attachment. `local_key` is AES(32) ‖ HMAC(32), the reverse of the main file."""
    if len(local_key) != 64:
        raise BackupDecryptError("attachment key has the wrong length")
    aes_key, mac_key = local_key[:32], local_key[32:]
    _verify_hmac(source, mac_key)
    digest = hashlib.sha256()
    remaining = size
    for chunk in _decrypt_cbc_stream(source, aes_key):
        # The plaintext is padded with zeros beyond `size`.
        chunk = chunk[:remaining]
        remaining -= len(chunk)
        digest.update(chunk)
        if chunk:
            yield chunk
    if remaining or not hmac.compare_digest(digest.digest(), plaintext_hash):
        raise BackupDecryptError("attachment content doesn't match its hash")


def _sho_squeeze(label: bytes, data: bytes, length: int) -> bytes:
    """libsignal poksho's ShoHmacSha256: new(label), absorb_and_ratchet(data), squeeze(length)."""

    def absorb_and_ratchet(cv: bytes, inp: bytes) -> bytes:
        return hmac.new(cv, inp + b"\x00", hashlib.sha256).digest()

    cv = absorb_and_ratchet(bytes(32), label)
    cv = absorb_and_ratchet(cv, data)
    out = b""
    i = 0
    while len(out) < length:
        out += hmac.new(cv, i.to_bytes(8, "big") + b"\x01", hashlib.sha256).digest()
        i += 1
    return out[:length]


def group_id_from_master_key(master_key: bytes) -> bytes:
    """The GroupIdentifier signal-cli reports (base64) for a group, derived like zkgroup's GroupSecretParams."""
    return _sho_squeeze(
        b"Signal_ZKGroup_20200424_GroupMasterKey_GroupSecretParams_DeriveFromMasterKey", master_key, 32
    )
