#!/bin/bash
aws ecs create-service \
  --cluster rategate \
  --service-name rategate-svc \
  --task-definition rategate \
  --desired-count 1 \
  --launch-type FARGATE \
  --network-configuration "awsvpcConfiguration={subnets=[subnet-0eed9b1ab1afb826a],securityGroups=[sg-0cf192b501edde9a6],assignPublicIp=ENABLED}" \
  --region us-east-1
