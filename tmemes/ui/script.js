(function () {
  // globals required by draw()
  let ctx;
  let canvas;
  let fallback;

  function readTextValues() {
    const top = document.getElementById("top").value;
    const bottom = document.getElementById("bottom").value;
    const anon = document.getElementById("anon").checked;
    overlays = [];
    if (top !== "") {
      overlays.push({
        text: top,
        field: {
          x: 0.5,
          y: 0.15,
          width: 1,
        },
        color: "white",
      });
    }
    if (bottom !== "") {
      overlays.push({
        text: bottom,
        field: {
          x: 0.5,
          y: 0.85,
          width: 1,
        },
        color: "white",
      });
    }
    return { overlays, anon };
  }

  function draw(e) {
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    let x = readTextValues();
    for (let overlay of x.overlays) {
      const field = overlay.field;
      const text = overlay.text;
      const x = field.x * fallback.naturalWidth;
      const y = field.y * fallback.naturalHeight;
      const width = field.width * fallback.naturalWidth;
      ctx.fillStyle = overlay.color;
      ctx.fillText(text, x, y, width);
    }
  }

  function submitMacro(id) {
    values = readTextValues();
    fetch(`/create/${id}`, {
      method: "POST",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
      },
      body: JSON.stringify(values),
    })
      .then(function (response) {
        response.json().then((data) => {
          window.location.href = "/macros/" + data.createdId;
        });
      })
      .catch(function (err) {
        console.log(`error encountered creating macro: ${err}`);
      });
  }

  function deleteMacro(id) {
    if (
      confirm(
        `Are you sure you want delete macro ID #${id}? This cannot be undone`
      )
    ) {
      fetch(`/api/macro/${id}`, {
        method: "DELETE",
        headers: {
          Accept: "application/json",
          "Content-Type": "application/json",
        },
      })
        .then(function () {
          window.location.href = "/";
        })
        .catch(function (err) {
          console.log(`error encountered deleting macro: ${err}`);
        });
    }
  }

  function deleteTemplate(id) {
    if (
      confirm(
        `Are you sure you want delete template ID #${id}? This cannot be undone`
      )
    ) {
      fetch(`/api/template/${id}`, {
        method: "DELETE",
        headers: {
          Accept: "application/json",
          "Content-Type": "application/json",
        },
      })
        .then(function () {
          window.location.href = "/templates";
        })
        .catch(function (err) {
          alert(`error encountered deleting template: ${err}`);
        });
    }
  }

  function setupCreatePage() {
    // setup submit button
    const submitBtn = document.getElementById("submit");
    const pathParts = window.location.pathname.split("/");
    const id = pathParts[pathParts.length - 1];
    submitBtn.addEventListener("click", () => {
      const warn =
        "Are you sure you want to submit this? Your coworkers will see it!";
      if (confirm(warn)) {
        submitMacro(id);
      }
    });
    // TODO more graceful fallback for gifs.
    canvas = document.getElementById("preview");
    fallback = document.getElementById("preview-fallback");
    canvas.width = fallback.naturalWidth;
    canvas.height = fallback.naturalHeight;

    // To match *tmemeServer.fontForImage. It's not exactly the same, but close enough for now.
    const typeHeightFraction = 0.15;
    let fontSize = fallback.naturalHeight * 0.75 * typeHeightFraction;

    ctx = canvas.getContext("2d");
    ctx.lineWidth = 1;
    ctx.textAlign = "center";
    ctx.font = `${fontSize}px Oswald SemiBold`;

    document.getElementById("top").addEventListener("input", draw);
    document.getElementById("bottom").addEventListener("input", draw);
    draw();
  }

  function setupListPages() {
    // setup delete buttons
    const deleteMacros = document.querySelectorAll("button.delete.macro");
    const deleteTemplates = document.querySelectorAll("button.delete.template");
    const upvoteMacros = document.querySelectorAll("button.upvote.macro");
    const downvoteMacros = document.querySelectorAll("button.downvote.macro");

    for (let i = 0; i < deleteMacros.length; i++) {
      const el = deleteMacros[i];
      el.addEventListener("click", () => {
        id = el.getAttribute("delete-id");
        deleteMacro(id);
      });
    }

    for (let i = 0; i < deleteTemplates.length; i++) {
      const el = deleteTemplates[i];
      el.addEventListener("click", () => {
        id = el.getAttribute("delete-id");
        deleteTemplate(id);
      });
    }

    for (let i = 0; i < upvoteMacros.length; i++) {
      const upEl = upvoteMacros[i];
      const downEl = downvoteMacros[i];
      upEl.addEventListener("click", () => {
        id = upEl.getAttribute("upvote-id");
        if (upEl.classList.contains("upvoted")) {
          unvoteMacro(id, upEl, downEl);
          return;
        }
        upvoteMacro(id, upEl, downEl);
      });
    }

    for (let i = 0; i < downvoteMacros.length; i++) {
      const upEl = upvoteMacros[i];
      const downEl = downvoteMacros[i];
      downEl.addEventListener("click", () => {
        id = downEl.getAttribute("downvote-id");
        if (downEl.classList.contains("downvoted")) {
          unvoteMacro(id, upEl, downEl);
          return;
        }
        downvoteMacro(id, upEl, downEl);
      });
    }
  }

  function updateVotes(upvoteElement, downvoteElement, data) {
    upvoteElement.innerHTML = data.upvotes ? data.upvotes : 0;
    downvoteElement.innerHTML = data.downvotes ? data.downvotes : 0;
  }

  function unvoteMacro(id, upvoteElement, downvoteElement) {
    fetch(`/api/vote/${id}`, {
      method: "DELETE",
    })
      .then(function (response) {
        response.json().then((data) => {
          upvoteElement.classList.remove("upvoted");
          downvoteElement.classList.remove("downvoted");
          updateVotes(upvoteElement, downvoteElement, data);
        });
      })
      .catch(function (err) {
        console.log(`error encountered deleting macro: ${err}`);
      });
  }

  function downvoteMacro(id, upvoteElement, downvoteElement) {
    fetch(`/api/macro/${id}/downvote`, {
      method: "PUT",
    })
      .then(function (response) {
        response.json().then((data) => {
          upvoteElement.classList.remove("upvoted");
          downvoteElement.classList.add("downvoted");
          updateVotes(upvoteElement, downvoteElement, data);
        });
      })
      .catch(function (err) {
        console.log(`error encountered downvoting macro: ${err}`);
      });
  }

  function upvoteMacro(id, upvoteElement, downvoteElement) {
    fetch(`/api/macro/${id}/upvote`, {
      method: "PUT",
    })
      .then(function (response) {
        response.json().then((data) => {
          upvoteElement.classList.add("upvoted");
          downvoteElement.classList.remove("downvoted");
          updateVotes(upvoteElement, downvoteElement, data);
        });
      })
      .catch(function (err) {
        console.log(`error encountered upvoting macro: ${err}`);
      });
  }

  function setup() {
    const page = document.body.getAttribute("id");
    switch (page) {
      case "templates":
      case "macros":
        setupListPages();
        break;
      case "create":
        setupCreatePage();
        break;
    }
  }
  setup();
})();