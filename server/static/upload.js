document.body.addEventListener("drop", e => { e.preventDefault(); e.stopPropagation(); });
document.body.addEventListener("dragenter", e => { e.preventDefault(); e.stopPropagation(); });
document.body.addEventListener("dragover", e => { e.preventDefault(); e.stopPropagation(); });
document.body.addEventListener("dragleave", e => { e.preventDefault(); e.stopPropagation(); });
document.body.addEventListener("dragenter", () => { document.body.classList.add("highlight"); });
document.body.addEventListener("dragover", () => { document.body.classList.add("highlight"); });
document.body.addEventListener("drop", () => { document.body.classList.remove("highlight"); });
document.body.addEventListener("dragleave", () => { document.body.classList.remove("highlight"); });

function preview() {
  const [file] = document.getElementById("image").files;
  if (file) {
    document.getElementById("image-preview").src = URL.createObjectURL(file);
    document.getElementById("name").value = file.name;
  }
}

document.body.addEventListener("drop", e => {
  if (e.dataTransfer) {
    document.getElementById("image").files = e.dataTransfer.files;
    preview();
  }
});
document.getElementById("image").addEventListener("change", preview);
