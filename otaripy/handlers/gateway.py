# otaripy's handler: it comes with the build, as handlers/gateway.py beside the
# binary. pyre reads @llm and @route off it at Initialize; the work is in the
# otaripy module the plugin's Python carries.

import otaripy

# TODO(IMPLEMENTME): GET /v1/models is not served. openrouter-qntx served it,
# and ground code's prompt.d (MODELS_PATH) reads each model's context_length
# from it. Nothing calls it on otaripy today.


@llm()
def chat(request):
    return otaripy.chat(request, plugin_config)


@route('POST', '/v1/chat/completions')
def completions(request):
    return otaripy.completions(request, plugin_config)


@route('GET', '/usage')
def usage(request):
    return otaripy.usage(request, plugin_config)
