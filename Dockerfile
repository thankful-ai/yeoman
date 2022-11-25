FROM python:3-alpine3.16

WORKDIR /opt/app/

COPY . /opt/app/

ENTRYPOINT ["python", "./server.py"]
