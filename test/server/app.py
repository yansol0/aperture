from flask import Flask, request, jsonify
import sqlite3
import os

app = Flask(__name__)

DB_PATH = os.environ.get("DB_PATH", "/app/app.db")
API_KEYS = {
	"KEY_ALICE": "alice",
	"KEY_BOB": "bob",
}


def get_db_connection():
	conn = sqlite3.connect(DB_PATH)
	conn.row_factory = sqlite3.Row
	return conn


def init_db():
	os.makedirs(os.path.dirname(DB_PATH), exist_ok=True)
	conn = get_db_connection()
	cur = conn.cursor()
	cur.execute(
		"""
		CREATE TABLE IF NOT EXISTS notes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			owner TEXT NOT NULL,
			title TEXT NOT NULL,
			content TEXT
		)
		"""
	)
	# Seed data if empty
	cur.execute("SELECT COUNT(*) as c FROM notes")
	count = cur.fetchone()[0]
	if count == 0:
		cur.execute("INSERT INTO notes (owner, title, content) VALUES (?, ?, ?)", ("alice", "Alice Private Note", "secret A"))
		cur.execute("INSERT INTO notes (owner, title, content) VALUES (?, ?, ?)", ("bob", "Bob Private Note", "secret B"))
	conn.commit()
	conn.close()


def get_auth_user():
	api_key = request.headers.get("X-API-Key")
	if not api_key:
		return None
	return API_KEYS.get(api_key)


@app.get("/health")
def health():
	return jsonify({"status": "ok"}), 200


# VULNERABLE: requires an API key but does NOT verify ownership of the note
@app.get("/notes/<int:note_id>")
def get_note(note_id: int):
	user = get_auth_user()
	if user is None:
		return jsonify({"error": "unauthorized"}), 401
	conn = get_db_connection()
	cur = conn.cursor()
	cur.execute("SELECT id, owner, title, content FROM notes WHERE id = ?", (note_id,))
	row = cur.fetchone()
	conn.close()
	if not row:
		return jsonify({"error": "not_found"}), 404
	return jsonify({"id": row[0], "owner": row[1], "title": row[2], "content": row[3]}), 200


# SECURE: only allow listing notes for the same user as the API key
@app.get("/users/<user_id>/notes")
def list_user_notes(user_id: str):
	user = get_auth_user()
	if user is None:
		return jsonify({"error": "unauthorized"}), 401
	if user != user_id:
		return jsonify({"error": "forbidden"}), 403
	conn = get_db_connection()
	cur = conn.cursor()
	cur.execute("SELECT id, owner, title, content FROM notes WHERE owner = ? ORDER BY id", (user_id,))
	rows = cur.fetchall()
	conn.close()
	notes = [{"id": r[0], "owner": r[1], "title": r[2], "content": r[3]} for r in rows]
	return jsonify(notes), 200


# SECURE: create a note owned by the authenticated user
@app.post("/notes")
def create_note():
	user = get_auth_user()
	if user is None:
		return jsonify({"error": "unauthorized"}), 401
	data = request.get_json(silent=True) or {}
	title = data.get("title")
	content = data.get("content", "")
	if not title:
		return jsonify({"error": "title_required"}), 400
	conn = get_db_connection()
	cur = conn.cursor()
	cur.execute("INSERT INTO notes (owner, title, content) VALUES (?, ?, ?)", (user, title, content))
	note_id = cur.lastrowid
	conn.commit()
	conn.close()
	return jsonify({"id": int(note_id), "owner": user, "title": title, "content": content}), 201


# VULNERABLE: allow creating a note for ANY owner provided in the body
@app.post("/notes/vuln-owner")
def create_note_vuln_owner():
	user = get_auth_user()
	if user is None:
		return jsonify({"error": "unauthorized"}), 401
	data = request.get_json(silent=True) or {}
	owner = data.get("owner")
	title = data.get("title")
	content = data.get("content", "")
	if not owner or not title:
		return jsonify({"error": "owner_and_title_required"}), 400
	conn = get_db_connection()
	cur = conn.cursor()
	cur.execute("INSERT INTO notes (owner, title, content) VALUES (?, ?, ?)", (owner, title, content))
	note_id = cur.lastrowid
	conn.commit()
	conn.close()
	return jsonify({"id": int(note_id), "owner": owner, "title": title, "content": content}), 201


# SECURE: accept owner in body but enforce it must match the authenticated user
@app.post("/notes/secure-owner")
def create_note_secure_owner():
	user = get_auth_user()
	if user is None:
		return jsonify({"error": "unauthorized"}), 401
	data = request.get_json(silent=True) or {}
	owner = data.get("owner")
	title = data.get("title")
	content = data.get("content", "")
	if not owner or not title:
		return jsonify({"error": "owner_and_title_required"}), 400
	if owner != user:
		return jsonify({"error": "forbidden_owner_mismatch"}), 403
	conn = get_db_connection()
	cur = conn.cursor()
	cur.execute("INSERT INTO notes (owner, title, content) VALUES (?, ?, ?)", (user, title, content))
	note_id = cur.lastrowid
	conn.commit()
	conn.close()
	return jsonify({"id": int(note_id), "owner": user, "title": title, "content": content}), 201


# SECURE: update a note only if the authenticated user currently owns it; owner change must
# either be absent or equal to the authenticated user
@app.put("/notes/<int:note_id>")
def update_note_secure(note_id: int):
	user = get_auth_user()
	if user is None:
		return jsonify({"error": "unauthorized"}), 401
	data = request.get_json(silent=True) or {}
	conn = get_db_connection()
	cur = conn.cursor()
	cur.execute("SELECT id, owner, title, content FROM notes WHERE id = ?", (note_id,))
	row = cur.fetchone()
	if not row:
		conn.close()
		return jsonify({"error": "not_found"}), 404
	current_owner = row[1]
	if current_owner != user:
		conn.close()
		return jsonify({"error": "forbidden"}), 403
	new_owner = data.get("owner", current_owner)
	if new_owner != user:
		conn.close()
		return jsonify({"error": "forbidden_owner_change"}), 403
	new_title = data.get("title", row[2])
	new_content = data.get("content", row[3])
	cur.execute(
		"UPDATE notes SET owner = ?, title = ?, content = ? WHERE id = ?",
		(new_owner, new_title, new_content, note_id),
	)
	conn.commit()
	cur.execute("SELECT id, owner, title, content FROM notes WHERE id = ?", (note_id,))
	row2 = cur.fetchone()
	conn.close()
	return jsonify({"id": row2[0], "owner": row2[1], "title": row2[2], "content": row2[3]}), 200


# VULNERABLE: update a note without verifying ownership; allows changing owner arbitrarily
@app.put("/notes/<int:note_id>/vuln")
def update_note_vuln(note_id: int):
	user = get_auth_user()
	if user is None:
		return jsonify({"error": "unauthorized"}), 401
	data = request.get_json(silent=True) or {}
	conn = get_db_connection()
	cur = conn.cursor()
	cur.execute("SELECT id, owner, title, content FROM notes WHERE id = ?", (note_id,))
	row = cur.fetchone()
	if not row:
		conn.close()
		return jsonify({"error": "not_found"}), 404
	new_owner = data.get("owner", row[1])
	new_title = data.get("title", row[2])
	new_content = data.get("content", row[3])
	cur.execute(
		"UPDATE notes SET owner = ?, title = ?, content = ? WHERE id = ?",
		(new_owner, new_title, new_content, note_id),
	)
	conn.commit()
	cur.execute("SELECT id, owner, title, content FROM notes WHERE id = ?", (note_id,))
	row2 = cur.fetchone()
	conn.close()
	return jsonify({"id": row2[0], "owner": row2[1], "title": row2[2], "content": row2[3]}), 200


# VULNERABLE: allow delete without verifying ownership
@app.delete("/notes/<int:note_id>")
def delete_note(note_id: int):
	user = get_auth_user()
	if user is None:
		return jsonify({"error": "unauthorized"}), 401
	conn = get_db_connection()
	cur = conn.cursor()
	cur.execute("DELETE FROM notes WHERE id = ?", (note_id,))
	deleted = cur.rowcount
	conn.commit()
	conn.close()
	if deleted == 0:
		return jsonify({"error": "not_found"}), 404
	return ("", 204)


if __name__ == "__main__":
	init_db()
	port = int(os.environ.get("PORT", "8080"))
	app.run(host="0.0.0.0", port=port) 