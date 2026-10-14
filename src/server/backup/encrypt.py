import hashlib
import hmac
import os
import zlib
from collections.abc import Iterable
from pathlib import Path

import attr
from cryptography.hazmat.primitives import padding
from cryptography.hazmat.primitives.ciphers import Cipher
from cryptography.hazmat.primitives.ciphers import algorithms
from cryptography.hazmat.primitives.ciphers import modes

from server.backup import crypto
from server.backup.proto import LocalBackup_pb2

_CHUNK = 1 << 20


def varint(n: int) -> bytes:
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def _write_cbc_hmac(out_path: Path, aes_key: bytes, mac_key: bytes, chunks: Iterable[bytes]) -> None:
    """Write `IV(16) ‖ AES-256-CBC-PKCS7(chunks) ‖ HMAC-SHA256(IV ‖ ciphertext)`, streaming."""
    iv = os.urandom(16)
    encryptor = Cipher(algorithms.AES(aes_key), modes.CBC(iv)).encryptor()
    padder = padding.PKCS7(128).padder()
    mac = hmac.new(mac_key, digestmod=hashlib.sha256)
    tmp = out_path.with_name(out_path.name + ".partial")
    with tmp.open("wb") as f:
        f.write(iv)
        mac.update(iv)
        for chunk in chunks:
            ct = encryptor.update(padder.update(chunk))
            f.write(ct)
            mac.update(ct)
        ct = encryptor.update(padder.finalize()) + encryptor.finalize()
        f.write(ct)
        mac.update(ct)
        f.write(mac.digest())
    tmp.rename(out_path)


@attr.s(auto_attribs=True, frozen=True)
class EncryptedAttachment:
    """Locator fields for a FilePointer pointing at an attachment written into a local backup's `files/` dir."""

    local_key: bytes
    plaintext_hash: bytes
    size: int


def _file_chunks(path: Path) -> Iterable[bytes]:
    with path.open("rb") as f:
        while chunk := f.read(_CHUNK):
            yield chunk


def write_attachment(files_dir: Path, source: Path) -> EncryptedAttachment:
    """Encrypt `source` into `files_dir/<xx>/<mediaName>`, as the apps' local backups do."""
    digest = hashlib.sha256()
    for chunk in _file_chunks(source):
        digest.update(chunk)
    plaintext_hash = digest.digest()
    local_key = os.urandom(64)
    name = crypto.local_media_name(plaintext_hash, local_key)
    dest = files_dir / name[:2] / name
    dest.parent.mkdir(parents=True, exist_ok=True)
    # local_key is AES(32) ‖ HMAC(32), the reverse of the main file's key order.
    _write_cbc_hmac(dest, local_key[:32], local_key[32:], _file_chunks(source))
    return EncryptedAttachment(local_key=local_key, plaintext_hash=plaintext_hash, size=source.stat().st_size)


def write_main(out_path: Path, hmac_key: bytes, aes_key: bytes, frames: Iterable[bytes]) -> None:
    """Write a backup `main` file: varint-delimited frames, gzipped, then encrypted and MACed."""

    def gzipped() -> Iterable[bytes]:
        deflater = zlib.compressobj(wbits=31)
        buf = bytearray()
        for frame in frames:
            buf += varint(len(frame)) + frame
            if len(buf) >= _CHUNK:
                yield deflater.compress(bytes(buf))
                buf.clear()
        yield deflater.compress(bytes(buf)) + deflater.flush()

    _write_cbc_hmac(out_path, aes_key, hmac_key, gzipped())


def write_metadata(out_path: Path, bk: bytes, backup_id: bytes) -> None:
    iv = os.urandom(12)
    encryptor = Cipher(algorithms.AES(crypto.local_metadata_key(bk)), modes.CTR(iv + b"\0\0\0\0")).encryptor()
    metadata = LocalBackup_pb2.Metadata(version=1)
    metadata.backupId.iv = iv
    metadata.backupId.encryptedId = encryptor.update(backup_id) + encryptor.finalize()
    out_path.write_bytes(metadata.SerializeToString())
