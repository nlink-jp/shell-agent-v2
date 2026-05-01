FROM python:3.12-slim

# CJK fonts — without these, matplotlib renders Japanese /
# Chinese / Korean labels as tofu (□□). fonts-noto-cjk
# covers the basic CJK ranges; -extra adds variants.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        fonts-noto-cjk \
        fonts-noto-cjk-extra \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Common analysis libraries the model is likely to reach
# for during data work.
RUN pip install --no-cache-dir \
        pandas \
        numpy \
        matplotlib \
        scipy \
        scikit-learn

# matplotlib rcParams: put Noto Sans CJK JP into the font
# fallback chain so charts with Japanese labels render
# correctly even when neither user nor model explicitly
# sets rcParams['font.family'].
COPY matplotlibrc /etc/matplotlib/matplotlibrc
ENV MATPLOTLIBRC=/etc/matplotlib/matplotlibrc

WORKDIR /work
