import os
import time
import random
import socket
import re
from collections import Counter
from flask import Flask, request, jsonify

app = Flask(__name__)


@app.route("/health", methods=["GET"])
def health():
    return jsonify({"status": "ok"}), 200


@app.route("/process", methods=["POST"])
def process():
    data = request.get_json(force=True)
    job_id = data.get("job_id", "unknown")
    payload = data.get("payload", {})

    app.logger.info("processing job_id=%s", job_id)

    text = payload.get("text", "")
    if not isinstance(text, str):
        return jsonify({"success": False, "error": "payload.text must be a string"}), 200

    start = time.time()

    # Simulate processing time 0.5-2 seconds.
    time.sleep(random.uniform(0.5, 2.0))

    # Word count.
    words = text.split()
    word_count = len(words)

    # Character count.
    char_count = len(text)

    # Sentence count (split on .!?).
    sentences = re.split(r"[.!?]+", text)
    sentence_count = len([s for s in sentences if s.strip()])

    # Top words (frequency analysis, case-insensitive, top 10).
    cleaned = re.sub(r"[^a-zA-Z\s]", "", text.lower())
    word_freq = Counter(cleaned.split())
    top_words = dict(word_freq.most_common(10))

    # Reversed text.
    reversed_text = text[::-1]

    processing_ms = int((time.time() - start) * 1000)

    result = {
        "word_count": word_count,
        "character_count": char_count,
        "sentence_count": sentence_count,
        "top_words": top_words,
        "reversed_text": reversed_text,
        "processing_ms": processing_ms,
        "worker_hostname": socket.gethostname(),
    }

    app.logger.info("job_id=%s completed in %dms", job_id, processing_ms)
    return jsonify({"success": True, "result": result}), 200


if __name__ == "__main__":
    port = int(os.environ.get("PORT", 8080))
    app.run(host="0.0.0.0", port=port)
