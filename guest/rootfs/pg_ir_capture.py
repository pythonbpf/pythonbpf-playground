# Imported by every Python process in the playground VM (via pg_ir_capture.pth;
# Ubuntu's own sitecustomize.py would shadow a sitecustomize of ours).
# When the agent sets PG_IR_OUT, copy the LLVM IR that PythonBPF generates to
# that path so the UI can show it in the "LLVM IR" tab. No effect otherwise.
import os

_out = os.environ.get("PG_IR_OUT")
if _out:
    try:
        import shutil

        import pythonbpf
        import pythonbpf.codegen as _cg

        _orig = _cg.compile_to_ir

        def compile_to_ir(filename, output, *args, **kwargs):
            try:
                return _orig(filename, output, *args, **kwargs)
            finally:
                try:
                    shutil.copyfile(output, _out + ".tmp")
                    os.replace(_out + ".tmp", _out)
                except OSError:
                    pass

        _cg.compile_to_ir = compile_to_ir
        pythonbpf.compile_to_ir = compile_to_ir
    except Exception:  # never break the user's program over the IR tab
        pass
