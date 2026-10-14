import sqlite3

import attr

_DISPLAY_NAME_SQL = (
    "coalesce(nullif({r}.contact_name, ''), nullif({r}.profile_name, ''), nullif({r}.group_title, ''), {r}.e164, "
    "CASE {r}.kind WHEN 'group' THEN 'Unnamed group' ELSE 'Unknown' END)"
)


def display_name_sql(alias: str) -> str:
    return _DISPLAY_NAME_SQL.format(r=alias)


@attr.s(auto_attribs=True, frozen=True)
class ConversationSummary:
    id: int
    recipient_id: int
    kind: str
    name: str
    e164: str | None
    last_message_at: int
    last_message_preview: str | None
    unread_count: int
    expire_timer_s: int
    archived: bool
    pinned: bool


@attr.s(auto_attribs=True, frozen=True)
class AttachmentView:
    id: int
    content_type: str | None
    filename: str | None
    size: int | None
    width: int | None
    height: int | None
    caption: str | None
    is_voice_note: bool
    status: str


@attr.s(auto_attribs=True, frozen=True)
class ReactionView:
    emoji: str
    reactor_id: int
    reactor_name: str


@attr.s(auto_attribs=True, frozen=True)
class QuoteView:
    author_name: str
    sent_at: int
    text: str | None
    message_id: int | None


@attr.s(auto_attribs=True, frozen=True)
class MessageView:
    id: int
    author_id: int
    author_name: str
    outgoing: bool
    sent_at: int
    kind: str
    body: str | None
    body_ranges: str | None
    quote: QuoteView | None
    edited_at: int | None
    deleted: bool
    expires_at: int | None
    view_once: bool
    viewed: bool
    update_payload: str | None
    status: str | None
    attachments: list[AttachmentView]
    reactions: list[ReactionView]


@attr.s(auto_attribs=True, frozen=True)
class MessagePage:
    messages: list[MessageView]
    has_more: bool


@attr.s(auto_attribs=True, frozen=True)
class ConversationMember:
    recipient_id: int
    name: str
    e164: str | None
    aci: str | None
    role: str


@attr.s(auto_attribs=True, frozen=True)
class ConversationDetail:
    summary: ConversationSummary
    members: list[ConversationMember]


@attr.s(auto_attribs=True, frozen=True)
class SearchHit:
    message_id: int
    conversation_id: int
    conversation_name: str
    author_name: str
    sent_at: int
    snippet: str


def _summary_select(where: str) -> str:
    return f"""
        SELECT c.id, c.recipient_id, r.kind, {display_name_sql("r")} AS name, r.e164, c.last_message_at,
               c.expire_timer_s, c.archived, c.pinned,
               (SELECT CASE WHEN m.deleted_at IS NOT NULL THEN 'This message was deleted'
                            WHEN m.kind = 'update' THEN '(update)'
                            ELSE coalesce(m.body, '(attachment)') END
                  FROM messages m WHERE m.conversation_id = c.id ORDER BY m.sent_at DESC LIMIT 1) AS preview,
               (SELECT count(*) FROM messages m WHERE m.conversation_id = c.id AND m.sent_at > c.last_read_at
                  AND m.author_id != (SELECT id FROM recipients WHERE kind = 'self') AND m.kind = 'text') AS unread
        FROM conversations c JOIN recipients r ON r.id = c.recipient_id
        {where}
    """


def _summary(row: sqlite3.Row) -> ConversationSummary:
    return ConversationSummary(
        id=row["id"],
        recipient_id=row["recipient_id"],
        kind=row["kind"],
        name="Note to self" if row["kind"] == "self" else row["name"],
        e164=row["e164"],
        last_message_at=row["last_message_at"],
        last_message_preview=row["preview"],
        unread_count=row["unread"],
        expire_timer_s=row["expire_timer_s"],
        archived=bool(row["archived"]),
        pinned=bool(row["pinned"]),
    )


def list_conversations(conn: sqlite3.Connection) -> list[ConversationSummary]:
    rows = conn.execute(
        _summary_select("WHERE c.last_message_at > 0 OR r.kind = 'group'")
        + " ORDER BY c.pinned DESC, c.last_message_at DESC"
    ).fetchall()
    return [_summary(r) for r in rows]


def conversation_detail(conn: sqlite3.Connection, conversation_id: int) -> ConversationDetail | None:
    row = conn.execute(_summary_select("WHERE c.id = ?"), (conversation_id,)).fetchone()
    if row is None:
        return None
    members = [
        ConversationMember(recipient_id=m["id"], name=m["name"], e164=m["e164"], aci=m["aci"], role=m["role"])
        for m in conn.execute(
            f"SELECT r.id, {display_name_sql('r')} AS name, r.e164, r.aci, gm.role FROM group_members gm "
            "JOIN recipients r ON r.id = gm.member_recipient_id WHERE gm.group_recipient_id = ? ORDER BY name",
            (row["recipient_id"],),
        )
    ]
    return ConversationDetail(summary=_summary(row), members=members)


def list_contacts(conn: sqlite3.Connection) -> list[ConversationMember]:
    return [
        ConversationMember(recipient_id=r["id"], name=r["name"], e164=r["e164"], aci=r["aci"], role="contact")
        for r in conn.execute(
            f"SELECT id, {display_name_sql('recipients')} AS name, e164, aci FROM recipients "
            "WHERE kind = 'contact' AND (contact_name IS NOT NULL OR profile_name IS NOT NULL) ORDER BY name"
        )
    ]


def messages_page(
    conn: sqlite3.Connection, conversation_id: int, before_sent_at: int | None, limit: int
) -> MessagePage:
    self_row = conn.execute("SELECT id FROM recipients WHERE kind = 'self'").fetchone()
    self_id = None if self_row is None else self_row["id"]
    rows = conn.execute(
        f"""
        SELECT m.*, {display_name_sql("a")} AS author_name, {display_name_sql("qa")} AS quote_author_name,
               (SELECT q.id FROM messages q WHERE q.author_id = m.quote_author_id AND q.sent_at = m.quote_sent_at)
                 AS quote_message_id
        FROM messages m
        JOIN recipients a ON a.id = m.author_id
        LEFT JOIN recipients qa ON qa.id = m.quote_author_id
        WHERE m.conversation_id = ? AND m.sent_at < ?
        ORDER BY m.sent_at DESC LIMIT ?
        """,
        (conversation_id, before_sent_at if before_sent_at is not None else 2**62, limit + 1),
    ).fetchall()
    has_more = len(rows) > limit
    rows = list(reversed(rows[:limit]))
    ids = [r["id"] for r in rows]
    if not ids:
        return MessagePage(messages=[], has_more=False)
    placeholders = ",".join("?" * len(ids))

    attachments: dict[int, list[AttachmentView]] = {}
    for a in conn.execute(f"SELECT * FROM attachments WHERE message_id IN ({placeholders}) ORDER BY ordinal", ids):
        attachments.setdefault(a["message_id"], []).append(
            AttachmentView(
                id=a["id"],
                content_type=a["content_type"],
                filename=a["filename"],
                size=a["size"],
                width=a["width"],
                height=a["height"],
                caption=a["caption"],
                is_voice_note=bool(a["is_voice_note"]),
                status=a["status"],
            )
        )
    reactions: dict[int, list[ReactionView]] = {}
    for x in conn.execute(
        f"SELECT x.message_id, x.emoji, x.reactor_id, {display_name_sql('r')} AS name FROM reactions x "
        f"JOIN recipients r ON r.id = x.reactor_id WHERE x.message_id IN ({placeholders}) ORDER BY x.sent_at",
        ids,
    ):
        name = "You" if x["reactor_id"] == self_id else x["name"]
        reactions.setdefault(x["message_id"], []).append(
            ReactionView(emoji=x["emoji"], reactor_id=x["reactor_id"], reactor_name=name)
        )
    statuses: dict[int, str] = {}
    for s in conn.execute(
        f"""
        SELECT message_id,
               min(CASE status WHEN 'sent' THEN 0 WHEN 'delivered' THEN 1 WHEN 'read' THEN 2 ELSE 3 END) AS rank
        FROM receipts WHERE message_id IN ({placeholders}) GROUP BY message_id
        """,
        ids,
    ):
        statuses[s["message_id"]] = ["sent", "delivered", "read", "viewed"][s["rank"]]

    views = []
    for r in rows:
        outgoing = r["author_id"] == self_id
        quote = None
        if r["quote_sent_at"] is not None:
            quote = QuoteView(
                author_name="You" if r["quote_author_id"] == self_id else (r["quote_author_name"] or "Unknown"),
                sent_at=r["quote_sent_at"],
                text=r["quote_text"],
                message_id=r["quote_message_id"],
            )
        views.append(
            MessageView(
                id=r["id"],
                author_id=r["author_id"],
                author_name="You" if outgoing else r["author_name"],
                outgoing=outgoing,
                sent_at=r["sent_at"],
                kind=r["kind"],
                body=r["body"],
                body_ranges=r["body_ranges"],
                quote=quote,
                edited_at=r["edited_at"],
                deleted=r["deleted_at"] is not None,
                expires_at=r["expires_at"],
                view_once=bool(r["view_once"]),
                viewed=r["viewed_at"] is not None,
                update_payload=r["update_payload"],
                status=statuses.get(r["id"]) if outgoing else None,
                attachments=attachments.get(r["id"], []),
                reactions=reactions.get(r["id"], []),
            )
        )
    return MessagePage(messages=views, has_more=has_more)


def search(conn: sqlite3.Connection, query: str, limit: int) -> list[SearchHit]:
    # Quote each term so user input can't be parsed as FTS5 syntax.
    fts_query = " ".join('"' + term.replace('"', '""') + '"' for term in query.split())
    if not fts_query:
        return []
    rows = conn.execute(
        f"""
        SELECT m.id, m.conversation_id, m.sent_at, {display_name_sql("cr")} AS conversation_name,
               cr.kind AS conversation_kind, {display_name_sql("a")} AS author_name, a.kind AS author_kind,
               snippet(messages_fts, 0, '[', ']', '…', 12) AS snippet
        FROM messages_fts
        JOIN messages m ON m.id = messages_fts.rowid
        JOIN conversations c ON c.id = m.conversation_id
        JOIN recipients cr ON cr.id = c.recipient_id
        JOIN recipients a ON a.id = m.author_id
        WHERE messages_fts MATCH ?
        ORDER BY m.sent_at DESC LIMIT ?
        """,
        (fts_query, limit),
    ).fetchall()
    return [
        SearchHit(
            message_id=r["id"],
            conversation_id=r["conversation_id"],
            conversation_name="Note to self" if r["conversation_kind"] == "self" else r["conversation_name"],
            author_name="You" if r["author_kind"] == "self" else r["author_name"],
            sent_at=r["sent_at"],
            snippet=r["snippet"],
        )
        for r in rows
    ]


def attachment_file(conn: sqlite3.Connection, attachment_id: int) -> tuple[str, str | None, str | None] | None:
    row = conn.execute(
        "SELECT file_sha256, content_type, filename FROM attachments WHERE id = ? AND status = 'present'",
        (attachment_id,),
    ).fetchone()
    if row is None or row["file_sha256"] is None:
        return None
    return row["file_sha256"], row["content_type"], row["filename"]


def message_context(conn: sqlite3.Connection, message_id: int) -> tuple[int, int] | None:
    """(conversation_id, sent_at) for jumping to a message, eg from search."""
    row = conn.execute("SELECT conversation_id, sent_at FROM messages WHERE id = ?", (message_id,)).fetchone()
    return None if row is None else (row["conversation_id"], row["sent_at"])
